#include "bridge_client.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_client.h"
#include "sdkconfig.h"
#include "wifi.h"

enum { RESPONSE_MAX = 32 * 1024 };

typedef struct {
    char *data;
    size_t len;
    bool oversized;
} response_t;

static esp_err_t on_response(esp_http_client_event_t *event)
{
    response_t *response = event->user_data;
    if (event->event_id != HTTP_EVENT_ON_DATA || !response) {
        return ESP_OK;
    }
    if ((size_t)event->data_len >= RESPONSE_MAX - response->len) {
        response->oversized = true;
    }
    if (!response->oversized) {
        memcpy(response->data + response->len, event->data, event->data_len);
        response->len += event->data_len;
        response->data[response->len] = '\0';
    }
    return ESP_OK;
}

static esp_err_t post(const char *url, const char *content_type, const void *body, size_t bytes,
                      response_t *response, char *error, size_t error_len)
{
    const esp_http_client_config_t config = {
        .url = url,
        .method = HTTP_METHOD_POST,
        .timeout_ms = 60000,
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

    esp_err_t err = esp_http_client_set_header(client, "Content-Type", content_type);
    if (err == ESP_OK) {
        err = esp_http_client_set_header(client, "X-EchoEar-Token", CONFIG_ECHOEAR_BRIDGE_TOKEN);
    }
    if (err == ESP_OK) {
        err = esp_http_client_set_header(client, "X-Device-Id", device_id);
    }
    if (err == ESP_OK) {
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

esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, bridge_reply_t *result, char *error, size_t error_len)
{
    *result = (bridge_reply_t){ 0 };
    response_t response = { .data = heap_caps_malloc(RESPONSE_MAX, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT) };
    if (!response.data) {
        snprintf(error, error_len, "Not enough memory for bridge reply");
        return ESP_ERR_NO_MEM;
    }
    response.data[0] = '\0';
    esp_err_t err = post(CONFIG_ECHOEAR_BRIDGE_URL "/api/voice", "audio/pcm",
                         pcm, bytes, &response, error, error_len);
    if (err == ESP_OK) {
        if (response.oversized) {
            snprintf(error, error_len, "Bridge reply is too large");
            err = ESP_ERR_INVALID_SIZE;
        } else {
            cJSON *json = cJSON_ParseWithLengthOpts(response.data, response.len + 1, NULL, true);
            const cJSON *transcript = cJSON_GetObjectItemCaseSensitive(json, "transcript");
            const cJSON *reply = cJSON_GetObjectItemCaseSensitive(json, "reply");
            if (!cJSON_IsString(transcript) || !cJSON_IsString(reply)) {
                cJSON_Delete(json);
                snprintf(error, error_len, "Bridge response is missing transcript or reply");
                err = ESP_ERR_INVALID_RESPONSE;
            } else {
                const cJSON *screen = cJSON_GetObjectItemCaseSensitive(json, "screen");
                *result = (bridge_reply_t){
                    .json = json,
                    .transcript = transcript->valuestring,
                    .reply = reply->valuestring,
                    .screen = cJSON_IsObject(screen) ? screen : NULL,
                };
            }
        }
    }
    free(response.data);
    return err;
}

esp_err_t bridge_reset(char *error, size_t error_len)
{
    return post(CONFIG_ECHOEAR_BRIDGE_URL "/api/reset", "application/json",
                "{}", 2, NULL, error, error_len);
}
