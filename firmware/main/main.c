#include "audio.h"
#include "bridge_client.h"
#include "ota_server.h"
#include "ui.h"
#include "wifi.h"

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "nvs_flash.h"

static QueueHandle_t actions;
static TaskHandle_t worker;

static bool handle_action(ui_action_t action)
{
    if (action == UI_STOP_RECORDING) {
        xTaskNotifyGive(worker);
        return true;
    }
    return xQueueSend(actions, &action, 0) == pdPASS;
}

static bool stop_requested(void)
{
    return ulTaskNotifyTake(pdTRUE, 0) > 0;
}

static void on_wifi(bool connected, const char *ip)
{
    ui_set_network(connected, ip);
    if (connected) {
        ota_confirm_boot();
    }
}

static void conversation_task(void *arg)
{
    for (;;) {
        ui_action_t action;
        xQueueReceive(actions, &action, portMAX_DELAY);
        char error[128];
        if (action == UI_NEW_CHAT) {
            bool reset = bridge_reset(error, sizeof(error)) == ESP_OK;
            ui_set_phase(UI_IDLE, reset ? "New chat ready" : error);
            continue;
        }

        audio_tone(AUDIO_START_TONE);
        const int16_t *pcm;
        size_t bytes;
        esp_err_t err = audio_record(stop_requested, &pcm, &bytes);
        ui_set_phase(UI_BUSY, "Sending…");
        // A stop tap can race the recording limit. Don't carry it into the next recording.
        stop_requested();
        audio_tone(AUDIO_STOP_TONE);
        if (err != ESP_OK) {
            ui_set_phase(UI_IDLE, "Microphone read failed");
        } else if (bytes < AUDIO_RATE * sizeof(*pcm) / 5) {
            ui_set_phase(UI_IDLE, "Recording was too short. Try again.");
        } else {
            ui_set_phase(UI_BUSY, "Thinking…");
            bridge_reply_t result;
            if (bridge_voice(pcm, bytes, &result, error, sizeof(error)) == ESP_OK) {
                audio_tone(AUDIO_REPLY_TONE);
                ui_show_reply(result.transcript, result.reply, result.screen);
                cJSON_Delete(result.json);
            } else {
                ui_set_phase(UI_IDLE, error);
            }
        }
    }
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
    actions = xQueueCreate(1, sizeof(ui_action_t));
    ESP_ERROR_CHECK(actions ? ESP_OK : ESP_ERR_NO_MEM);
    ESP_ERROR_CHECK(xTaskCreate(conversation_task, "conversation", 8192, NULL, 5, &worker) == pdPASS
                    ? ESP_OK : ESP_ERR_NO_MEM);
    ESP_ERROR_CHECK(ui_init(device_id + sizeof("echoear-") - 1, handle_action));
    ui_set_audio_ready(audio_init() == ESP_OK);
    ESP_ERROR_CHECK(wifi_start(on_wifi));
    ESP_ERROR_CHECK(ota_server_start());
    ESP_LOGI("echoear", "Initialization complete for %s (%s)", device_id, device_mac);
}
