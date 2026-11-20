#include "head_touch.h"

#include "audio.h"
#include "bsp/esp_vocat.h"
#include "conversation.h"
#include "driver/touch_sens.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "ui.h"

enum { PADS = 2, SAMPLE_MS = 20, CALIBRATION_SAMPLES = 12, GESTURE_MS = 450 };
static const char *TAG = "head_touch";

static esp_err_t read_pad(touch_channel_handle_t pad, uint32_t *value)
{
    uint32_t data[TOUCH_SAMPLE_CFG_NUM];
    esp_err_t err = touch_channel_read_data(pad, TOUCH_CHAN_DATA_TYPE_SMOOTH, data);
    if (err == ESP_OK) {
        *value = data[0];
    }
    return err;
}

static void step_volume(int direction)
{
    const int levels[] = { 45, 65, 80, 95 };
    int current = audio_get_volume();
    int target = direction > 0 ? levels[3] : levels[0];
    for (int i = 0; i < 4; ++i) {
        int level = levels[direction > 0 ? i : 3 - i];
        if ((direction > 0 && level > current) || (direction < 0 && level < current)) {
            target = level;
            break;
        }
    }
    if (audio_set_volume(target) == ESP_OK) {
        ui_show_volume(target);
        conversation_chime();
    }
}

static void scan(touch_channel_handle_t pads[PADS], uint32_t baseline[PADS])
{
    bool pressed[PADS] = { 0 };
    unsigned debounce[PADS] = { 0 };
    int first = -1;
    int64_t start = 0;
    bool handled = false;
    bool woke = false;
    for (;;) {
        vTaskDelay(pdMS_TO_TICKS(SAMPLE_MS));
        int64_t now = esp_timer_get_time() / 1000;
        for (int i = 0; i < PADS; ++i) {
            uint32_t value;
            if (read_pad(pads[i], &value) != ESP_OK) {
                continue;
            }
            uint32_t delta = value > baseline[i] ? value - baseline[i] : baseline[i] - value;
            // The BSP uses a 1.5% deviation. Use a slightly higher press edge and a lower release edge.
            bool down = (uint64_t)delta * 1000 > (uint64_t)baseline[i] * (pressed[i] ? 11 : 18);
            if (down == pressed[i]) {
                debounce[i] = 0;
                if (!down && value) {
                    baseline[i] = ((uint64_t)baseline[i] * 31 + value) / 32;
                }
                continue;
            }
            if (++debounce[i] < 2) {
                continue;
            }
            debounce[i] = 0;
            pressed[i] = down;
            if (!down || handled) {
                continue;
            }
            if (first < 0) {
                first = i;
                start = now;
                woke = ui_wake();
            } else if (first != i && now - start <= GESTURE_MS) {
                step_volume(i > first ? 1 : -1);
                handled = true;
            }
        }
        if (first >= 0 && !handled && now - start >= GESTURE_MS) {
            if (pressed[first]) {
                step_volume(first == 1 ? 1 : -1);
            } else if (!woke) {
                ui_pet();
            }
            handled = true;
        }
        if (handled && !pressed[0] && !pressed[1]) {
            first = -1;
            handled = false;
        }
    }
}

static void head_task(void *arg)
{
    static const int channels[PADS] = { BSP_BUTTON_1_CH, BSP_BUTTON_2_CH };
    touch_sensor_sample_config_t samples[] = {
        TOUCH_SENSOR_V2_DEFAULT_SAMPLE_CONFIG(500, TOUCH_VOLT_LIM_L_0V5, TOUCH_VOLT_LIM_H_2V2),
    };
    touch_sensor_config_t config = TOUCH_SENSOR_DEFAULT_BASIC_CONFIG(TOUCH_SAMPLE_CFG_NUM, samples);
    touch_channel_config_t channel = {
        .active_thresh = { 2000 },
        .charge_speed = TOUCH_CHARGE_SPEED_7,
        .init_charge_volt = TOUCH_INIT_CHARGE_VOLT_DEFAULT,
    };
    touch_sensor_filter_config_t filter = TOUCH_SENSOR_DEFAULT_FILTER_CONFIG();
    touch_sensor_handle_t sensor = NULL;
    touch_channel_handle_t pads[PADS] = { 0 };
    bool enabled = false, scanning = false;
    esp_err_t err = touch_sensor_new_controller(&config, &sensor);
    for (int i = 0; err == ESP_OK && i < PADS; ++i) {
        err = touch_sensor_new_channel(sensor, channels[i], &channel, &pads[i]);
    }
    if (err == ESP_OK) {
        err = touch_sensor_config_filter(sensor, &filter);
    }
    if (err == ESP_OK) {
        err = touch_sensor_enable(sensor);
        enabled = err == ESP_OK;
    }
    if (err == ESP_OK) {
        err = touch_sensor_start_continuous_scanning(sensor);
        scanning = err == ESP_OK;
    }
    uint32_t baseline[PADS] = { 0 };
    if (err == ESP_OK) {
        vTaskDelay(pdMS_TO_TICKS(100));
        uint64_t sum[PADS] = { 0 };
        for (int n = 0; err == ESP_OK && n < CALIBRATION_SAMPLES; ++n) {
            vTaskDelay(pdMS_TO_TICKS(SAMPLE_MS));
            for (int i = 0; err == ESP_OK && i < PADS; ++i) {
                uint32_t value;
                err = read_pad(pads[i], &value);
                if (err == ESP_OK && value) {
                    sum[i] += value;
                } else if (err == ESP_OK) {
                    err = ESP_ERR_INVALID_RESPONSE;
                }
            }
        }
        if (err == ESP_OK) {
            for (int i = 0; i < PADS; ++i) {
                baseline[i] = sum[i] / CALIBRATION_SAMPLES;
            }
            ESP_LOGI(TAG, "Calibrated ESP32-S3 head pads %d and %d", channels[0], channels[1]);
            scan(pads, baseline);
        }
    }
    ESP_LOGW(TAG, "Head pads disabled: %s", esp_err_to_name(err));
    if (scanning) {
        touch_sensor_stop_continuous_scanning(sensor);
    }
    if (enabled) {
        touch_sensor_disable(sensor);
    }
    for (int i = 0; i < PADS; ++i) {
        if (pads[i]) {
            touch_sensor_del_channel(pads[i]);
        }
    }
    if (sensor) {
        touch_sensor_del_controller(sensor);
    }
    vTaskDelete(NULL);
}

esp_err_t head_touch_start(void)
{
    return xTaskCreate(head_task, "head_touch", 3072, NULL, 3, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}
