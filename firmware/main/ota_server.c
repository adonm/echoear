#include "ota_server.h"

#include <stdio.h>
#include <stdlib.h>

#include "audio.h"
#include "bridge_client.h"
#include "cat_face.h"
#include "conversation.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_http_server.h"
#include "esp_https_ota.h"
#include "esp_log.h"
#include "esp_ota_ops.h"
#include "esp_system.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "power.h"
#include "sdkconfig.h"
#include "ui.h"
#include "wifi.h"

static const char *TAG = "ota";

static esp_err_t text_response(httpd_req_t *request, const char *status, const char *message)
{
    httpd_resp_set_status(request, status);
    httpd_resp_set_type(request, "text/plain");
    return httpd_resp_sendstr(request, message);
}

static bool authorized(httpd_req_t *request)
{
    static const char expected[] = CONFIG_ECHOEAR_BRIDGE_TOKEN;
    char actual[sizeof(expected)];
    size_t len = httpd_req_get_hdr_value_len(request, "X-EchoEar-Token");
    if (len == 0 || len != sizeof(expected) - 1 ||
        httpd_req_get_hdr_value_str(request, "X-EchoEar-Token", actual, sizeof(actual)) != ESP_OK) {
        return false;
    }
    unsigned diff = 0;
    for (size_t i = 0; i < len; ++i) {
        diff |= (unsigned char)expected[i] ^ (unsigned char)actual[i];
    }
    return diff == 0;
}

static esp_err_t status_get(httpd_req_t *request)
{
    char ip[WIFI_IP_LEN];
    wifi_get_ip(ip);
    wifi_ap_record_t ap;
    int rssi = esp_wifi_sta_get_ap_info(&ap) == ESP_OK ? ap.rssi : 0;
    const esp_partition_t *running = esp_ota_get_running_partition();
    battery_status_t battery = battery_status();
    char pct[12] = "null", mv[16] = "null";
    if (battery.pct >= 0) {
        snprintf(pct, sizeof(pct), "%d", battery.pct);
        snprintf(mv, sizeof(mv), "%u", battery.millivolts);
    }
    char peer[BRIDGE_TARGET_CAP], peer_json[BRIDGE_TARGET_CAP + 2] = "null";
    bridge_client_get_peer(peer);
    if (peer[0]) {
        snprintf(peer_json, sizeof(peer_json), "\"%s\"", peer);
    }
    char json[448];
    snprintf(json, sizeof(json),
             "{\"device\":\"%s\",\"character\":\"%s\",\"mac\":\"%s\",\"ip\":\"%s\",\"wifi_rssi\":%d,"
             "\"free_heap\":%zu,\"free_psram\":%zu,\"firmware\":\"%s\",\"running_partition\":\"%s\","
             "\"volume\":%d,\"battery_pct\":%s,\"battery_mv\":%s,\"charging\":%s,\"sleeping\":%s,\"peer\":%s}",
             device_id, cat_face_name(), device_mac, ip, rssi,
             heap_caps_get_free_size(MALLOC_CAP_INTERNAL | MALLOC_CAP_8BIT),
             heap_caps_get_free_size(MALLOC_CAP_SPIRAM), esp_app_get_description()->version, running->label,
             audio_get_volume(), pct, mv, battery.charging ? "true" : "false",
             ui_is_sleeping() ? "true" : "false", peer_json);
    httpd_resp_set_type(request, "application/json");
    return httpd_resp_sendstr(request, json);
}

static esp_err_t respond_and_reboot(httpd_req_t *request, const char *message)
{
    esp_err_t err = text_response(request, "200 OK", message);
    // Give TCP a chance to transmit the response before resetting the interface.
    vTaskDelay(pdMS_TO_TICKS(500));
    esp_restart();
    return err;
}

static esp_err_t reboot_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    return respond_and_reboot(request, "Rebooting\n");
}

static esp_err_t display_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    ui_wake();
    const size_t bytes = request->content_len;
    if (!bytes) {
        return text_response(request, "400 Bad Request", "Expected a screen JSON object\n");
    }
    if (bytes > 8192) {
        return text_response(request, "413 Content Too Large", "Screen JSON exceeds 8192 bytes\n");
    }
    char *body = malloc(bytes + 1);
    if (!body) {
        return text_response(request, "503 Service Unavailable", "Could not allocate request buffer\n");
    }
    for (size_t offset = 0; offset < bytes;) {
        int count = httpd_req_recv(request, body + offset, bytes - offset);
        if (count <= 0) {
            free(body);
            text_response(request, "408 Request Timeout", "Incomplete screen JSON\n");
            return ESP_FAIL;
        }
        offset += count;
    }
    body[bytes] = '\0';
    cJSON *screen = cJSON_ParseWithLengthOpts(body, bytes + 1, NULL, true);
    free(body);
    esp_err_t err = ui_show_screen(screen);
    cJSON_Delete(screen);
    if (err != ESP_OK) {
        return text_response(request, "400 Bad Request", "Expected a screen object with up to 24 shapes\n");
    }
    return text_response(request, "200 OK", "Screen shown\n");
}

static esp_err_t display_clear_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    ui_hide_screen();
    return text_response(request, "200 OK", "Screen cleared\n");
}

static esp_err_t speak_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    size_t bytes = request->content_len;
    if (!bytes || bytes % sizeof(int16_t)) {
        return text_response(request, "400 Bad Request", "Expected 16-bit mono PCM\n");
    }
    if (bytes > 1536 * 1024) {
        return text_response(request, "413 Content Too Large", "PCM exceeds 1.5 MiB\n");
    }
    ui_wake();
    uint8_t *pcm = heap_caps_malloc(bytes, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!pcm) {
        return text_response(request, "503 Service Unavailable", "Could not allocate speech buffer\n");
    }
    for (size_t offset = 0; offset < bytes;) {
        int count = httpd_req_recv(request, (char *)pcm + offset, bytes - offset);
        if (count <= 0) {
            free(pcm);
            text_response(request, "408 Request Timeout", "Incomplete PCM upload\n");
            return ESP_FAIL;
        }
        offset += count;
    }
    size_t samples = bytes / sizeof(int16_t);
    esp_err_t err = conversation_submit_speech((int16_t *)pcm, samples);
    if (err != ESP_OK) {
        free(pcm);
        return err == ESP_ERR_NOT_SUPPORTED
               ? text_response(request, "503 Service Unavailable", "Speaker unavailable\n")
               : text_response(request, "409 Conflict", "EchoEar is busy\n");
    }
    char json[48];
    snprintf(json, sizeof(json), "{\"ok\":true,\"samples\":%zu}", samples);
    httpd_resp_set_type(request, "application/json");
    return httpd_resp_sendstr(request, json);
}

static esp_err_t volume_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    char body[64];
    size_t bytes = request->content_len;
    if (!bytes || bytes >= sizeof(body)) {
        return text_response(request, "400 Bad Request", "Expected {\"level\":0..100}\n");
    }
    for (size_t offset = 0; offset < bytes;) {
        int count = httpd_req_recv(request, body + offset, bytes - offset);
        if (count <= 0) {
            text_response(request, "408 Request Timeout", "Incomplete volume request\n");
            return ESP_FAIL;
        }
        offset += count;
    }
    body[bytes] = '\0';
    cJSON *json = cJSON_ParseWithLengthOpts(body, bytes + 1, NULL, true);
    const cJSON *level = cJSON_GetObjectItemCaseSensitive(json, "level");
    bool valid = cJSON_IsNumber(level) && level->valueint >= 0 && level->valueint <= 100 &&
                 level->valuedouble == level->valueint;
    int value = valid ? level->valueint : -1;
    cJSON_Delete(json);
    if (!valid) {
        return text_response(request, "400 Bad Request", "Expected an integer level from 0 to 100\n");
    }
    if (audio_set_volume(value) != ESP_OK) {
        return text_response(request, "503 Service Unavailable", "Could not save volume\n");
    }
    ui_show_volume(value);
    conversation_chime();
    char reply[32];
    snprintf(reply, sizeof(reply), "{\"ok\":true,\"volume\":%d}", value);
    httpd_resp_set_type(request, "application/json");
    return httpd_resp_sendstr(request, reply);
}

static esp_err_t pull_headers(esp_http_client_handle_t client)
{
    return esp_http_client_set_header(client, "X-EchoEar-Token", CONFIG_ECHOEAR_BRIDGE_TOKEN);
}

static esp_err_t ota_pull_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    const esp_http_client_config_t http = {
        .url = CONFIG_ECHOEAR_BRIDGE_URL "/ota/latest.bin",
        .crt_bundle_attach = esp_crt_bundle_attach,
        .timeout_ms = 15000,
        .disable_auto_redirect = true,
    };
    const esp_https_ota_config_t config = {
        .http_config = &http, .http_client_init_cb = pull_headers,
    };
    esp_err_t err = esp_https_ota(&config);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Pull failed: %s", esp_err_to_name(err));
        return text_response(request, "502 Bad Gateway", "OTA pull failed\n");
    }
    return respond_and_reboot(request, "OTA installed; rebooting\n");
}

static esp_err_t ota_upload_post(httpd_req_t *request)
{
    if (!authorized(request)) {
        return text_response(request, "401 Unauthorized", "Invalid device token\n");
    }
    const esp_partition_t *target = esp_ota_get_next_update_partition(NULL);
    if (!target) {
        return text_response(request, "500 Internal Server Error", "No OTA partition\n");
    }
    if (!request->content_len || request->content_len > target->size) {
        return text_response(request, "400 Bad Request", "Invalid image size\n");
    }
    esp_ota_handle_t update;
    esp_err_t err = esp_ota_begin(target, request->content_len, &update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Begin upload: %s", esp_err_to_name(err));
        return text_response(request, "500 Internal Server Error", "Could not start OTA\n");
    }

    char chunk[2048];
    size_t remaining = request->content_len;
    while (remaining > 0) {
        int len = httpd_req_recv(request, chunk, remaining < sizeof(chunk) ? remaining : sizeof(chunk));
        if (len <= 0) {
            esp_ota_abort(update);
            text_response(request, "408 Request Timeout", "Incomplete OTA upload\n");
            return ESP_FAIL;
        }
        err = esp_ota_write(update, chunk, len);
        if (err != ESP_OK) {
            esp_ota_abort(update);
            ESP_LOGE(TAG, "Write upload: %s", esp_err_to_name(err));
            text_response(request, "400 Bad Request", "OTA write failed or image is invalid\n");
            return ESP_FAIL;
        }
        remaining -= len;
    }
    err = esp_ota_end(update);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Verify upload: %s", esp_err_to_name(err));
        return text_response(request, "400 Bad Request", "Invalid firmware image\n");
    }
    err = esp_ota_set_boot_partition(target);
    if (err != ESP_OK) {
        ESP_LOGE(TAG, "Set boot partition: %s", esp_err_to_name(err));
        return text_response(request, "500 Internal Server Error", "Could not activate OTA image\n");
    }
    ESP_LOGI(TAG, "Validated %zu bytes in %s", request->content_len, target->label);
    return respond_and_reboot(request, "OTA installed; rebooting\n");
}

esp_err_t ota_server_start(void)
{
    static const httpd_uri_t routes[] = {
        { .uri = "/status", .method = HTTP_GET, .handler = status_get },
        { .uri = "/ota", .method = HTTP_POST, .handler = ota_pull_post },
        { .uri = "/ota/upload", .method = HTTP_POST, .handler = ota_upload_post },
        { .uri = "/reboot", .method = HTTP_POST, .handler = reboot_post },
        { .uri = "/display", .method = HTTP_POST, .handler = display_post },
        { .uri = "/display/clear", .method = HTTP_POST, .handler = display_clear_post },
        { .uri = "/speak", .method = HTTP_POST, .handler = speak_post },
        { .uri = "/volume", .method = HTTP_POST, .handler = volume_post },
    };
    httpd_config_t config = HTTPD_DEFAULT_CONFIG();
    config.stack_size = 8192;
    config.max_uri_handlers = sizeof(routes) / sizeof(routes[0]);
    config.recv_wait_timeout = 15;
    config.lru_purge_enable = true;
    httpd_handle_t server;
    esp_err_t err = httpd_start(&server, &config);
    if (err != ESP_OK) {
        return err;
    }
    for (size_t i = 0; i < sizeof(routes) / sizeof(routes[0]); ++i) {
        err = httpd_register_uri_handler(server, &routes[i]);
        if (err != ESP_OK) {
            httpd_stop(server);
            return err;
        }
    }
    ESP_LOGI(TAG, "Device HTTP server is listening on port %u", config.server_port);
    return ESP_OK;
}

void ota_confirm_boot(void)
{
    esp_ota_img_states_t state;
    if (esp_ota_get_state_partition(esp_ota_get_running_partition(), &state) == ESP_OK &&
        state == ESP_OTA_IMG_PENDING_VERIFY) {
        esp_err_t err = esp_ota_mark_app_valid_cancel_rollback();
        if (err == ESP_OK) {
            ESP_LOGI(TAG, "Confirmed OTA image after display and Wi-Fi initialized");
        } else {
            ESP_LOGE(TAG, "Confirm OTA image: %s", esp_err_to_name(err));
        }
    }
}
