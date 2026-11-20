#include "bridge_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "audio.h"
#include "conversation.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "sdkconfig.h"
#include "ui.h"
#include "wifi.h"

enum { RESPONSE_MAX = 32 * 1024, SPEECH_MAX = 2 * 1024 * 1024, SPEECH_CHUNK = 4096, PATH_MAX_LEN = 512 };
static const char *TAG = "bridge";

typedef struct {
    char *data;
    size_t len;
    size_t capacity;
    bool oversized;
    bool pcm_type;
} response_t;

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
    return err == ESP_OK ? esp_http_client_set_header(client, "X-Device-Id", device_id) : err;
}

static esp_err_t request(esp_http_client_method_t method, const char *url, const char *content_type,
                         const void *body, size_t bytes, response_t *response, char *error, size_t error_len)
{
    const esp_http_client_config_t config = {
        .url = url,
        .method = method,
        .timeout_ms = method == HTTP_METHOD_GET ? 15000 : 60000,
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
    if (err == ESP_OK && content_type) {
        err = esp_http_client_set_header(client, "Content-Type", content_type);
    }
    if (err == ESP_OK && method == HTTP_METHOD_POST) {
        err = esp_http_client_set_post_field(client, body, bytes);
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

static void speak_reply(const char *path, size_t bytes)
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
            err = conversation_stream_begin();
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

static esp_err_t handle_reply(cJSON *json, const char *question, char *error, size_t error_len)
{
    const cJSON *transcript = cJSON_GetObjectItemCaseSensitive(json, "transcript");
    const cJSON *reply = cJSON_GetObjectItemCaseSensitive(json, "reply");
    if (!cJSON_IsString(reply) || (!question && !cJSON_IsString(transcript))) {
        snprintf(error, error_len, "Bridge reply is incomplete");
        return ESP_ERR_INVALID_RESPONSE;
    }
    const cJSON *screen = cJSON_GetObjectItemCaseSensitive(json, "screen");
    ui_show_reply(cJSON_IsString(transcript) ? transcript->valuestring : question, reply->valuestring, screen);
    const cJSON *audio_url = cJSON_GetObjectItemCaseSensitive(json, "audio_url");
    const cJSON *audio_bytes = cJSON_GetObjectItemCaseSensitive(json, "audio_bytes");
    bool length_ok = !audio_bytes || cJSON_IsNull(audio_bytes) ||
                     (cJSON_IsNumber(audio_bytes) && audio_bytes->valuedouble == audio_bytes->valueint &&
                      audio_bytes->valueint >= 0 && audio_bytes->valueint <= SPEECH_MAX &&
                      audio_bytes->valueint % sizeof(int16_t) == 0);
    if (cJSON_IsString(audio_url) && audio_url->valuestring[0] && length_ok) {
        speak_reply(audio_url->valuestring, cJSON_IsNumber(audio_bytes) ? audio_bytes->valueint : 0);
    }
    return ESP_OK;
}

static esp_err_t ask(const char *url, const char *type, const void *body, size_t bytes,
                     const char *question, char *error, size_t error_len)
{
    response_t response = { .data = heap_caps_malloc(RESPONSE_MAX, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT),
                            .capacity = RESPONSE_MAX - 1 };
    if (!response.data) {
        snprintf(error, error_len, "Not enough memory for bridge reply");
        return ESP_ERR_NO_MEM;
    }
    esp_err_t err = request(HTTP_METHOD_POST, url, type, body, bytes, &response, error, error_len);
    cJSON *json = NULL;
    if (err == ESP_OK) {
        if (response.oversized) {
            snprintf(error, error_len, "Bridge reply is too large");
            err = ESP_ERR_INVALID_SIZE;
        } else {
            response.data[response.len] = '\0';
            json = cJSON_ParseWithLengthOpts(response.data, response.len + 1, NULL, true);
        }
    }
    free(response.data);
    if (err == ESP_OK) {
        err = handle_reply(json, question, error, error_len);
    }
    cJSON_Delete(json);
    return err;
}

esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, char *error, size_t error_len)
{
    return ask(CONFIG_ECHOEAR_BRIDGE_URL "/api/voice", "audio/pcm", pcm, bytes, NULL, error, error_len);
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
    esp_err_t err = ask(CONFIG_ECHOEAR_BRIDGE_URL "/api/text", "application/json",
                        body, strlen(body), question, error, sizeof(error));
    cJSON_free(body);
    if (err != ESP_OK) {
        ESP_LOGW(TAG, "Text follow-up: %s", error);
    }
    return err;
}

esp_err_t bridge_reset(char *error, size_t error_len)
{
    return request(HTTP_METHOD_POST, CONFIG_ECHOEAR_BRIDGE_URL "/api/reset", "application/json",
                   "{}", 2, NULL, error, error_len);
}
