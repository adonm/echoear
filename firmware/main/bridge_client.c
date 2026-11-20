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
       REGISTER_REPLY_MAX = 4096, REGISTER_INTERVAL_MS = 60000 };
static const char *TAG = "bridge";
static TaskHandle_t registrar;
static atomic_bool connected;
static portMUX_TYPE peer_lock = portMUX_INITIALIZER_UNLOCKED;
static char peer_name[BRIDGE_TARGET_CAP];

typedef struct {
    char *data;
    size_t len;
    size_t capacity;
    bool oversized;
    bool pcm_type;
} response_t;

typedef struct {
    const char *url;
    const char *content_type;
    const void *body;
    size_t bytes;
    const char *target;
    int timeout_ms;
} post_t;

bool bridge_client_valid_target(const char *target)
{
    if (!target || !*target) {
        return false;
    }
    for (size_t i = 0; target[i]; ++i) {
        const unsigned char c = target[i];
        // Names and device IDs also travel in HTTP headers and the public status JSON.
        if (i >= BRIDGE_TARGET_CAP - 1 || !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
                                            (c >= '0' && c <= '9') || c == '-')) {
            return false;
        }
    }
    return true;
}

void bridge_client_get_peer(char peer[BRIDGE_TARGET_CAP])
{
    taskENTER_CRITICAL(&peer_lock);
    memcpy(peer, peer_name, sizeof(peer_name));
    taskEXIT_CRITICAL(&peer_lock);
}

static void set_peer(const char *name)
{
    char next[BRIDGE_TARGET_CAP] = "";
    snprintf(next, sizeof(next), "%s", name);
    taskENTER_CRITICAL(&peer_lock);
    bool changed = strcmp(peer_name, next) != 0;
    memcpy(peer_name, next, sizeof(peer_name));
    taskEXIT_CRITICAL(&peer_lock);
    if (changed) {
        ui_set_peer(next);
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

static esp_err_t device_headers(esp_http_client_handle_t client)
{
    esp_err_t err = esp_http_client_set_header(client, "X-EchoEar-Token", CONFIG_ECHOEAR_BRIDGE_TOKEN);
    if (err == ESP_OK) {
        err = esp_http_client_set_header(client, "X-Device-Id", device_id);
    }
    return err == ESP_OK ? esp_http_client_set_header(client, "X-Character", cat_face_name()) : err;
}

static esp_err_t post(post_t req, response_t *response, char *error, size_t error_len)
{
    const esp_http_client_config_t config = {
        .url = req.url,
        .method = HTTP_METHOD_POST,
        .timeout_ms = req.timeout_ms ? req.timeout_ms : 60000,
        .disable_auto_redirect = true,
        .event_handler = on_response,
        .user_data = response,
        .crt_bundle_attach = esp_crt_bundle_attach,
    };
    esp_http_client_handle_t client = esp_http_client_init(&config);
    if (!client) {
        snprintf(error, error_len, "Could not initialize bridge connection");
        return ESP_ERR_NO_MEM;
    }

    esp_err_t err = device_headers(client);
    if (err == ESP_OK) {
        err = esp_http_client_set_header(client, "Content-Type", req.content_type);
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
    int status = esp_http_client_get_status_code(client);
    esp_http_client_cleanup(client);

    if (err != ESP_OK) {
        snprintf(error, error_len, "Bridge connection failed: %s", esp_err_to_name(err));
    } else if (status < 200 || status >= 300) {
        snprintf(error, error_len, "Bridge returned HTTP %d", status);
        err = ESP_FAIL;
    }
    return err;
}

static esp_err_t post_json(post_t req, size_t capacity, cJSON **json, char *error, size_t error_len)
{
    response_t response = { .data = heap_caps_malloc(capacity, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT),
                            .capacity = capacity - 1 };
    if (!response.data) {
        snprintf(error, error_len, "Not enough memory for bridge reply");
        return ESP_ERR_NO_MEM;
    }
    esp_err_t err = post(req, &response, error, error_len);
    if (err == ESP_OK) {
        if (response.oversized) {
            snprintf(error, error_len, "Bridge reply is too large");
            err = ESP_ERR_INVALID_SIZE;
        } else {
            response.data[response.len] = '\0';
            *json = cJSON_ParseWithLengthOpts(response.data, response.len + 1, NULL, true);
            if (!*json) {
                snprintf(error, error_len, "Bridge returned invalid JSON");
                err = ESP_ERR_INVALID_RESPONSE;
            }
        }
    }
    free(response.data);
    return err;
}

static const char *primary_peer(const cJSON *peers)
{
    const char *fallback = "";
    const cJSON *peer;
    cJSON_ArrayForEach(peer, peers) {
        const cJSON *id = cJSON_GetObjectItemCaseSensitive(peer, "device_id");
        const cJSON *name = cJSON_GetObjectItemCaseSensitive(peer, "character");
        if (!cJSON_IsString(id) || strncmp(id->valuestring, "echoear-", 8) != 0 ||
            strlen(id->valuestring) != 12 || strspn(id->valuestring + 8, "0123456789abcdefABCDEF") != 4 ||
            strcasecmp(id->valuestring, device_id) == 0 ||
            !cJSON_IsString(name) || !bridge_client_valid_target(name->valuestring)) {
            continue;
        }
        if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(peer, "online"))) {
            return name->valuestring;
        }
        if (!*fallback) {
            fallback = name->valuestring;
        }
    }
    return fallback;
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
        .url = CONFIG_ECHOEAR_BRIDGE_URL "/api/register", .content_type = "application/json",
        .body = body, .bytes = (size_t)bytes, .timeout_ms = 5000,
    };
    cJSON *json = NULL;
    char error[128];
    esp_err_t err = post_json(req, REGISTER_REPLY_MAX, &json, error, sizeof(error));
    const cJSON *peers = cJSON_GetObjectItemCaseSensitive(json, "peers");
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Registration: %s", error);
    } else if (!cJSON_IsArray(peers)) {
        ESP_LOGW(TAG, "Registration response has no peers array");
    } else if (atomic_load(&connected)) {
        set_peer(primary_peer(peers));
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
            set_peer("");
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

static void speak_reply(const char *path, size_t bytes, const char *caption)
{
    if (path[0] != '/' || path[1] == '/' || strnlen(path, PATH_MAX_LEN + 1) > PATH_MAX_LEN) {
        ESP_LOGW(TAG, "Ignored invalid speech path");
        return;
    }
    for (const unsigned char *c = (const unsigned char *)path; *c; ++c) {
        if (*c <= ' ' || *c == '\\' || *c == '#' || *c == 127) {
            ESP_LOGW(TAG, "Ignored invalid speech path");
            return;
        }
    }
    char url[sizeof(CONFIG_ECHOEAR_BRIDGE_URL) + PATH_MAX_LEN];
    snprintf(url, sizeof(url), "%s%s", CONFIG_ECHOEAR_BRIDGE_URL, path);
    uint8_t *buffer = heap_caps_malloc(SPEECH_CHUNK, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!buffer) {
        ESP_LOGW(TAG, "Could not allocate speech chunk");
        return;
    }
    response_t response = { 0 };
    const esp_http_client_config_t config = {
        .url = url, .method = HTTP_METHOD_GET, .timeout_ms = 15000,
        .disable_auto_redirect = true, .crt_bundle_attach = esp_crt_bundle_attach,
        .event_handler = on_response, .user_data = &response,
    };
    esp_http_client_handle_t client = esp_http_client_init(&config);
    esp_err_t err = client ? device_headers(client) : ESP_ERR_NO_MEM;
    if (err == ESP_OK) {
        err = esp_http_client_open(client, 0);
    }
    if (err == ESP_OK) {
        int64_t length = esp_http_client_fetch_headers(client);
        bool chunked = esp_http_client_is_chunked_response(client);
        int code = esp_http_client_get_status_code(client);
        if (length < 0 || code != 200 || !response.pcm_type ||
            (!chunked && (length == 0 || length > SPEECH_MAX || length % 2 || (bytes && length != bytes)))) {
            err = ESP_ERR_INVALID_RESPONSE;
        } else if (!chunked) {
            bytes = length;
        }
    }
    if (err == ESP_OK) {
        err = esp_http_client_set_timeout_ms(client, 400);
    }
    bool playing = false;
    size_t total = 0, carry = 0;
    int64_t last_data = esp_timer_get_time();
    while (err == ESP_OK && !(playing && audio_playback_stopped())) {
        int received = esp_http_client_read(client, (char *)buffer + carry, SPEECH_CHUNK - carry);
        if (received <= 0) {
            if (esp_http_client_is_complete_data_received(client)) {
                if (!total || carry || (bytes && total != bytes)) {
                    err = ESP_ERR_INVALID_RESPONSE;
                }
                break;
            }
            if (received == -ESP_ERR_HTTP_EAGAIN && esp_timer_get_time() - last_data < 8000000) {
                continue;
            }
            err = received == -ESP_ERR_HTTP_EAGAIN ? ESP_ERR_TIMEOUT : ESP_ERR_INVALID_RESPONSE;
            break;
        }
        last_data = esp_timer_get_time();
        total += received;
        if (total > (bytes ? bytes : SPEECH_MAX)) {
            err = ESP_ERR_INVALID_SIZE;
            break;
        }
        size_t available = carry + received;
        size_t samples = available / sizeof(int16_t);
        if (samples && !playing) {
            err = conversation_stream_begin(caption);
            playing = err == ESP_OK;
        }
        if (samples && err == ESP_OK) {
            err = audio_stream_write((const int16_t *)buffer, samples);
        }
        // Transfer chunks need not end at a 16-bit sample boundary.
        carry = available % sizeof(int16_t);
        if (carry) {
            buffer[0] = buffer[available - 1];
        }
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
    const cJSON *audio_url = cJSON_GetObjectItemCaseSensitive(json, "audio_url");
    const cJSON *audio_bytes = cJSON_GetObjectItemCaseSensitive(json, "audio_bytes");
    bool length_ok = !audio_bytes || cJSON_IsNull(audio_bytes) ||
                     (cJSON_IsNumber(audio_bytes) && audio_bytes->valuedouble == audio_bytes->valueint &&
                      audio_bytes->valueint >= 0 && audio_bytes->valueint <= SPEECH_MAX &&
                      audio_bytes->valueint % sizeof(int16_t) == 0);
    if (cJSON_IsString(audio_url) && audio_url->valuestring[0] && length_ok) {
        speak_reply(audio_url->valuestring, cJSON_IsNumber(audio_bytes) ? audio_bytes->valueint : 0, caption);
    }
}

static esp_err_t ask(post_t req, const char *question, char *error, size_t error_len)
{
    cJSON *json = NULL;
    esp_err_t err = post_json(req, RESPONSE_MAX, &json, error, error_len);
    if (err == ESP_OK) {
        const cJSON *transcript = cJSON_GetObjectItemCaseSensitive(json, "transcript");
        const cJSON *reply = cJSON_GetObjectItemCaseSensitive(json, "reply");
        if (!cJSON_IsString(reply) || (!question && !cJSON_IsString(transcript))) {
            snprintf(error, error_len, "Bridge reply is incomplete");
            err = ESP_ERR_INVALID_RESPONSE;
        } else {
            const cJSON *screen = cJSON_GetObjectItemCaseSensitive(json, "screen");
            ui_show_reply(cJSON_IsString(transcript) ? transcript->valuestring : question, reply->valuestring, screen);
            play_reply_audio(json, NULL);
        }
    }
    cJSON_Delete(json);
    return err;
}

esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, char *error, size_t error_len)
{
    return ask((post_t){ .url = CONFIG_ECHOEAR_BRIDGE_URL "/api/voice", .content_type = "audio/pcm",
                         .body = pcm, .bytes = bytes }, NULL, error, error_len);
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
    esp_err_t err = ask((post_t){ .url = CONFIG_ECHOEAR_BRIDGE_URL "/api/text", .content_type = "application/json",
                                 .body = body, .bytes = strlen(body) }, question, error, sizeof(error));
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
    const post_t req = { .url = CONFIG_ECHOEAR_BRIDGE_URL "/api/intercom", .content_type = "audio/pcm",
                         .body = pcm, .bytes = sample_count * sizeof(*pcm), .target = target };
    cJSON *json = NULL;
    char error[128];
    esp_err_t err = post_json(req, RESPONSE_MAX, &json, error, sizeof(error));
    if (err == ESP_OK) {
        const cJSON *reply = cJSON_GetObjectItemCaseSensitive(json, "reply");
        if (cJSON_IsString(reply) && *reply->valuestring) {
            const char *caption = strnlen(reply->valuestring, 65) <= 64 ? reply->valuestring : NULL;
            play_reply_audio(json, caption);
            if (cJSON_IsFalse(cJSON_GetObjectItemCaseSensitive(json, "ok"))) {
                snprintf(error, sizeof(error), "Bridge could not reach the peer");
                err = ESP_FAIL;
            }
        } else {
            snprintf(error, sizeof(error), "Bridge did not confirm delivery");
            err = ESP_ERR_INVALID_RESPONSE;
        }
    }
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Intercom: %s", error);
    }
    cJSON_Delete(json);
    return err;
}

esp_err_t bridge_reset(char *error, size_t error_len)
{
    return post((post_t){ .url = CONFIG_ECHOEAR_BRIDGE_URL "/api/reset", .content_type = "application/json",
                          .body = "{}", .bytes = 2 }, NULL, error, error_len);
}
