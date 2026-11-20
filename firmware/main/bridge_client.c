#include "bridge_client.h"

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "audio.h"
#include "cat_face.h"
#include "conversation.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "sdkconfig.h"
#include "ui.h"
#include "wifi.h"

enum { RESPONSE_MAX = 32 * 1024, SPEECH_MAX = 2 * 1024 * 1024, SPEECH_CHUNK = 4096, PATH_MAX_LEN = 512,
       REGISTER_REPLY_MAX = 4096, REGISTER_INTERVAL_MS = 60000, TURN_TIMEOUT_MS = 110000 };
static const char *TAG = "bridge";
static TaskHandle_t registrar;
static atomic_bool connected;
static portMUX_TYPE peer_lock = portMUX_INITIALIZER_UNLOCKED;
static bridge_peer_t primary;

typedef struct {
    char *data;
    size_t len;
    size_t capacity;
    bool oversized;
    bool pcm_type;
} response_t;

typedef struct {
    const char *path;
    const char *content_type;
    const void *body;
    size_t bytes;
    const char *target;
    int timeout_ms;
    size_t reply_capacity;
} post_t;

bool bridge_client_valid_target(const char *target)
{
    // Names and IDs must be safe for both HTTP headers and the public JSON status.
    size_t len = target ? strspn(target, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-") : 0;
    return len > 0 && len < BRIDGE_TARGET_CAP && target[len] == '\0';
}

bridge_peer_t bridge_client_get_peer(void)
{
    taskENTER_CRITICAL(&peer_lock);
    bridge_peer_t peer = primary;
    taskEXIT_CRITICAL(&peer_lock);
    return peer;
}

static void set_peer(bridge_peer_t next)
{
    taskENTER_CRITICAL(&peer_lock);
    bool changed = strcmp(primary.id, next.id) || strcmp(primary.character, next.character);
    primary = next;
    taskEXIT_CRITICAL(&peer_lock);
    if (changed) {
        ui_set_peer(next.id, next.character);
    }
}

static esp_err_t on_response(esp_http_client_event_t *event)
{
    response_t *response = event->user_data;
    if (!response) {
        return ESP_OK;
    }
    if (event->event_id == HTTP_EVENT_ON_HEADER && event->header_key && event->header_value &&
        strcasecmp(event->header_key, "Content-Type") == 0) {
        const char *type = event->header_value;
        response->pcm_type = strncasecmp(type, "audio/pcm", 9) == 0 && (type[9] == '\0' || type[9] == ';');
    } else if (event->event_id == HTTP_EVENT_ON_DATA && response->data) {
        if ((size_t)event->data_len > response->capacity - response->len) {
            response->oversized = true;
        }
        if (!response->oversized) {
            memcpy(response->data + response->len, event->data, event->data_len);
            response->len += event->data_len;
        }
    }
    return ESP_OK;
}

static esp_http_client_handle_t bridge_http_client(const char *path, esp_http_client_method_t method,
                                                   int timeout_ms, response_t *response, esp_err_t *err)
{
    char url[sizeof(CONFIG_ECHOEAR_BRIDGE_URL) + PATH_MAX_LEN];
    snprintf(url, sizeof(url), "%s%s", CONFIG_ECHOEAR_BRIDGE_URL, path);
    const esp_http_client_config_t config = {
        .url = url, .method = method, .timeout_ms = timeout_ms, .disable_auto_redirect = true,
        .crt_bundle_attach = esp_crt_bundle_attach, .event_handler = on_response, .user_data = response,
    };
    esp_http_client_handle_t client = esp_http_client_init(&config);
    *err = client ? ESP_OK : ESP_ERR_NO_MEM;
    const char *headers[][2] = {
        { "X-EchoEar-Token", CONFIG_ECHOEAR_BRIDGE_TOKEN },
        { "X-Device-Id", device_id },
        { "X-Character", cat_face_name() },
    };
    for (size_t i = 0; *err == ESP_OK && i < sizeof(headers) / sizeof(headers[0]); ++i) {
        *err = esp_http_client_set_header(client, headers[i][0], headers[i][1]);
    }
    return client;
}

static esp_err_t bridge_http_request(post_t req, cJSON **json, char *error, size_t error_len)
{
    response_t response = { 0 };
    if (json) {
        size_t capacity = req.reply_capacity ? req.reply_capacity : RESPONSE_MAX;
        *json = NULL;
        response.data = heap_caps_malloc(capacity, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
        response.capacity = capacity - 1;
        if (!response.data) {
            snprintf(error, error_len, "Not enough memory for bridge reply");
            return ESP_ERR_NO_MEM;
        }
    }
    esp_err_t err;
    esp_http_client_handle_t client = bridge_http_client(req.path, HTTP_METHOD_POST,
                                      req.timeout_ms ? req.timeout_ms : TURN_TIMEOUT_MS, &response, &err);
    if (err == ESP_OK) {
        err = esp_http_client_set_header(client, "Content-Type", req.content_type ? req.content_type : "application/json");
    }
    if (err == ESP_OK && req.target) {
        err = esp_http_client_set_header(client, "X-Target", req.target);
    }
    if (err == ESP_OK) {
        err = esp_http_client_set_post_field(client, req.body, req.bytes);
    }
    if (err == ESP_OK) {
        err = esp_http_client_perform(client);
    }
    int status = client ? esp_http_client_get_status_code(client) : 0;
    if (client) {
        esp_http_client_cleanup(client);
    }

    if (!client) {
        snprintf(error, error_len, "Could not initialize bridge connection");
    } else if (err != ESP_OK) {
        snprintf(error, error_len, "Bridge connection failed: %s", esp_err_to_name(err));
    } else if (status < 200 || status >= 300) {
        snprintf(error, error_len, "Bridge returned HTTP %d", status);
        err = ESP_FAIL;
    } else if (response.oversized) {
        snprintf(error, error_len, "Bridge reply is too large");
        err = ESP_ERR_INVALID_SIZE;
    } else if (json) {
        response.data[response.len] = '\0';
        *json = cJSON_ParseWithLengthOpts(response.data, response.len + 1, NULL, true);
        if (!*json) {
            snprintf(error, error_len, "Bridge returned invalid JSON");
            err = ESP_ERR_INVALID_RESPONSE;
        }
    }
    free(response.data);
    return err;
}

static const char *json_string(const cJSON *obj, const char *key)
{
    return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(obj, key));
}

static bridge_peer_t select_peer(const cJSON *peers)
{
    bridge_peer_t next = { 0 };
    const cJSON *peer;
    cJSON_ArrayForEach(peer, peers) {
        const char *id = json_string(peer, "device_id"), *name = json_string(peer, "character");
        if (!id || strncmp(id, "echoear-", 8) != 0 || strlen(id) != 12 ||
            strspn(id + 8, "0123456789abcdefABCDEF") != 4 || strcasecmp(id, device_id) == 0 ||
            !bridge_client_valid_target(name)) {
            continue;
        }
        bool online = cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(peer, "online"));
        if (online || !next.id[0]) {
            strcpy(next.id, id);
            strcpy(next.character, name);
        }
        if (online) {
            break;
        }
    }
    return next;
}

static void register_device(void)
{
    char ip[WIFI_IP_LEN];
    wifi_get_ip(ip);
    if (strcmp(ip, "0.0.0.0") == 0) {
        return;
    }
    char body[128];
    int bytes = snprintf(body, sizeof(body), "{\"device_id\":\"%s\",\"character\":\"%s\",\"ip\":\"%s\",\"port\":80}",
                         device_id, cat_face_name(), ip);
    if (bytes < 0 || (size_t)bytes >= sizeof(body)) {
        return;
    }
    const post_t req = {
        .path = "/api/register", .body = body, .bytes = (size_t)bytes,
        .timeout_ms = 5000, .reply_capacity = REGISTER_REPLY_MAX,
    };
    cJSON *json = NULL;
    char error[128];
    esp_err_t err = bridge_http_request(req, &json, error, sizeof(error));
    const cJSON *peers = cJSON_GetObjectItemCaseSensitive(json, "peers");
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Registration: %s", error);
    } else if (!cJSON_IsArray(peers)) {
        ESP_LOGW(TAG, "Registration response has no peers array");
    } else if (atomic_load(&connected)) {
        set_peer(select_peer(peers));
    }
    cJSON_Delete(json);
}

static void registration_task(void *arg)
{
    for (;;) {
        ulTaskNotifyTake(pdTRUE, atomic_load(&connected) ? pdMS_TO_TICKS(REGISTER_INTERVAL_MS) : portMAX_DELAY);
        if (atomic_load(&connected)) {
            register_device();
        } else {
            set_peer((bridge_peer_t){ 0 });
        }
    }
}

esp_err_t bridge_client_start_registration(void)
{
    if (!CONFIG_ECHOEAR_BRIDGE_URL[0]) {
        return ESP_OK;
    }
    return xTaskCreate(registration_task, "bridge_reg", 4096, NULL, 3, &registrar) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}

void bridge_client_network_changed(bool online)
{
    atomic_store(&connected, online);
    if (registrar) {
        xTaskNotifyGive(registrar);
    }
}

static bool valid_audio_path(const char *path)
{
    if (path[0] != '/' || path[1] == '/' || strnlen(path, PATH_MAX_LEN + 1) > PATH_MAX_LEN) {
        return false;
    }
    for (const unsigned char *c = (const unsigned char *)path; *c; ++c) {
        if (*c <= ' ' || *c == '\\' || *c == '#' || *c == 127) {
            return false;
        }
    }
    return true;
}

static esp_err_t stream_audio(esp_http_client_handle_t client, uint8_t *buffer, size_t bytes,
                              const char *caption, bool *playing)
{
    size_t total = 0, carry = 0;
    int64_t last_data = esp_timer_get_time();
    while (!*playing || !audio_playback_stopped()) {
        int received = esp_http_client_read(client, (char *)buffer + carry, SPEECH_CHUNK - carry);
        if (received <= 0) {
            if (esp_http_client_is_complete_data_received(client)) {
                return total && !carry && (!bytes || total == bytes) ? ESP_OK : ESP_ERR_INVALID_RESPONSE;
            }
            if (received == -ESP_ERR_HTTP_EAGAIN && esp_timer_get_time() - last_data < 8000000) {
                continue;
            }
            return received == -ESP_ERR_HTTP_EAGAIN ? ESP_ERR_TIMEOUT : ESP_ERR_INVALID_RESPONSE;
        }
        last_data = esp_timer_get_time();
        total += received;
        if (total > (bytes ? bytes : SPEECH_MAX)) {
            return ESP_ERR_INVALID_SIZE;
        }
        size_t available = carry + received;
        size_t samples = available / sizeof(int16_t);
        if (samples) {
            esp_err_t err = *playing ? ESP_OK : conversation_stream_begin(caption);
            if (err != ESP_OK) {
                return err;
            }
            *playing = true;
            err = audio_stream_write((const int16_t *)buffer, samples);
            if (err != ESP_OK) {
                return err;
            }
        }
        // Transfer chunks need not end at a 16-bit sample boundary.
        carry = available % sizeof(int16_t);
        if (carry) {
            buffer[0] = buffer[available - 1];
        }
    }
    return ESP_OK;
}

static void download_and_play_audio(const char *path, size_t bytes, const char *caption)
{
    if (!valid_audio_path(path)) {
        ESP_LOGW(TAG, "Ignored invalid speech path");
        return;
    }
    uint8_t *buffer = heap_caps_malloc(SPEECH_CHUNK, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!buffer) {
        ESP_LOGW(TAG, "Could not allocate speech chunk");
        return;
    }
    response_t response = { 0 };
    esp_err_t err;
    bool playing = false;
    esp_http_client_handle_t client = bridge_http_client(path, HTTP_METHOD_GET, 15000, &response, &err);
    if (err == ESP_OK) {
        err = esp_http_client_open(client, 0);
    }
    if (err == ESP_OK) {
        int64_t length = esp_http_client_fetch_headers(client);
        bool chunked = esp_http_client_is_chunked_response(client);
        if (length < 0 || esp_http_client_get_status_code(client) != 200 || !response.pcm_type ||
            (!chunked && (length == 0 || length > SPEECH_MAX || length % 2 || (bytes && (size_t)length != bytes)))) {
            err = ESP_ERR_INVALID_RESPONSE;
        } else if (!chunked) {
            bytes = length;
        }
    }
    if (err == ESP_OK) {
        err = esp_http_client_set_timeout_ms(client, 400);
    }
    if (err == ESP_OK) {
        err = stream_audio(client, buffer, bytes, caption, &playing);
    }
    if (client) {
        esp_http_client_close(client);
        esp_http_client_cleanup(client);
    }
    if (playing) {
        bool cancelled = audio_playback_stopped();
        esp_err_t end = conversation_stream_end();
        if (err == ESP_OK || cancelled) {
            err = end;
        }
    }
    free(buffer);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Speech stream: %s", esp_err_to_name(err));
    }
}

static void play_reply_audio(const cJSON *json, const char *caption)
{
    const char *audio_url = json_string(json, "audio_url");
    const cJSON *audio_bytes = cJSON_GetObjectItemCaseSensitive(json, "audio_bytes");
    bool length_ok = !audio_bytes || cJSON_IsNull(audio_bytes) ||
                     (cJSON_IsNumber(audio_bytes) && audio_bytes->valuedouble == audio_bytes->valueint &&
                      audio_bytes->valueint >= 0 && audio_bytes->valueint <= SPEECH_MAX &&
                      audio_bytes->valueint % sizeof(int16_t) == 0);
    if (audio_url && *audio_url && length_ok) {
        download_and_play_audio(audio_url, cJSON_IsNumber(audio_bytes) ? audio_bytes->valueint : 0, caption);
    }
}

static esp_err_t turn(post_t req, const char *question, char *error, size_t error_len)
{
    cJSON *json = NULL;
    esp_err_t err = bridge_http_request(req, &json, error, error_len);
    if (err == ESP_OK) {
        const char *transcript = json_string(json, "transcript"), *reply = json_string(json, "reply");
        if (!reply || (req.target ? !*reply : !question && !transcript)) {
            snprintf(error, error_len, "%s", req.target ? "Bridge did not confirm delivery" : "Bridge reply is incomplete");
            err = ESP_ERR_INVALID_RESPONSE;
        } else {
            if (!req.target) {
                ui_show_reply(transcript ? transcript : question, reply, cJSON_GetObjectItemCaseSensitive(json, "screen"));
            }
            play_reply_audio(json, req.target && strnlen(reply, 65) <= 64 ? reply : NULL);
            if (req.target && cJSON_IsFalse(cJSON_GetObjectItemCaseSensitive(json, "ok"))) {
                snprintf(error, error_len, "Bridge could not reach the peer");
                err = ESP_FAIL;
            }
        }
    }
    cJSON_Delete(json);
    return err;
}

esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, char *error, size_t error_len)
{
    return turn((post_t){ .path = "/api/voice", .content_type = "audio/pcm", .body = pcm, .bytes = bytes },
                NULL, error, error_len);
}

esp_err_t bridge_client_ask_text(const char *question)
{
    cJSON *json = cJSON_CreateObject();
    if (!json || !cJSON_AddStringToObject(json, "text", question) ||
        !cJSON_AddStringToObject(json, "device_id", device_id)) {
        cJSON_Delete(json);
        return ESP_ERR_NO_MEM;
    }
    char *body = cJSON_PrintUnformatted(json);
    cJSON_Delete(json);
    if (!body) {
        return ESP_ERR_NO_MEM;
    }
    char error[128];
    esp_err_t err = turn((post_t){ .path = "/api/text", .body = body, .bytes = strlen(body) },
                         question, error, sizeof(error));
    cJSON_free(body);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Text follow-up: %s", error);
    }
    return err;
}

esp_err_t bridge_client_send_intercom(const char *target, const int16_t *pcm, size_t sample_count)
{
    if (!bridge_client_valid_target(target) || !pcm || !sample_count || sample_count > AUDIO_RATE * AUDIO_MAX_SECONDS) {
        return ESP_ERR_INVALID_ARG;
    }
    const post_t req = { .path = "/api/intercom", .content_type = "audio/pcm",
                         .body = pcm, .bytes = sample_count * sizeof(*pcm), .target = target };
    char error[128];
    esp_err_t err = turn(req, NULL, error, sizeof(error));
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Intercom: %s", error);
    }
    return err;
}

esp_err_t bridge_reset(char *error, size_t error_len)
{
    return bridge_http_request((post_t){ .path = "/api/reset", .body = "{}", .bytes = 2 }, NULL, error, error_len);
}
