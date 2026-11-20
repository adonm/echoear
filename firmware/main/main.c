#include "audio.h"
#include "display.h"
#include "media.h"
#include "ota_server.h"
#include "power.h"
#include "wifi.h"
#include "ws_client.h"

#include "esp_log.h"
#include "nvs_flash.h"

static const char *TAG = "echoear";

static void on_wifi(bool online, const char *ip)
{
    ws_client_network_changed(online);
    if (online) ota_confirm_boot();
}

void app_main(void)
{
    esp_err_t nvs = nvs_flash_init();
    if (nvs == ESP_ERR_NVS_NO_FREE_PAGES || nvs == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        nvs = nvs_flash_init();
    }
    ESP_ERROR_CHECK(nvs);
    ESP_ERROR_CHECK(wifi_init_identity());
    ESP_ERROR_CHECK(media_init(audio_init() == ESP_OK));
    ESP_ERROR_CHECK(display_init());
    esp_err_t ws = ws_client_init();
    if (ws != ESP_OK) ESP_LOGW(TAG, "WebSocket unavailable: %s", esp_err_to_name(ws));
    if (power_start() != ESP_OK) ESP_LOGW(TAG, "Battery monitor unavailable");
    ESP_ERROR_CHECK(wifi_start(on_wifi));
    ESP_ERROR_CHECK(ota_server_start());
    ESP_LOGI(TAG, "%s ready", device_id);
}
