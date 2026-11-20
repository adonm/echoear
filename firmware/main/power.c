#include "power.h"

#include "board.h"

#if !BOARD_HAS_BATTERY
battery_status_t battery_status(void) { return (battery_status_t){ .pct = -1 }; }
esp_err_t power_start(void) { return ESP_OK; }
#else
#include "bsp/esp_vocat.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

enum { GAUGE_ADDRESS = 0x55, VOLTAGE = 0x08, AVERAGE_CURRENT = 0x14, RELATIVE_SOC = 0x2c };
static const char *TAG = "power";
static battery_status_t status = { .pct = -1 };
static portMUX_TYPE status_lock = portMUX_INITIALIZER_UNLOCKED;

battery_status_t battery_status(void)
{
    portENTER_CRITICAL(&status_lock);
    battery_status_t copy = status;
    portEXIT_CRITICAL(&status_lock);
    return copy;
}

static bool read_word(i2c_master_dev_handle_t gauge, uint8_t reg, uint16_t *word)
{
    uint8_t bytes[2];
    if (i2c_master_transmit_receive(gauge, &reg, 1, bytes, sizeof(bytes), 40) != ESP_OK) {
        return false;
    }
    *word = ((uint16_t)bytes[1] << 8) | bytes[0];
    return true;
}

static void power_task(void *arg)
{
    i2c_master_bus_handle_t bus = bsp_i2c_get_handle();
    const i2c_device_config_t config = { .dev_addr_length = I2C_ADDR_BIT_LEN_7,
                                        .device_address = GAUGE_ADDRESS, .scl_speed_hz = 100000 };
    i2c_master_dev_handle_t gauge;
    if (!bus || i2c_master_bus_add_device(bus, &config, &gauge) != ESP_OK) {
        ESP_LOGW(TAG, "Battery gauge bus unavailable");
        vTaskDelete(NULL);
        return;
    }
    bool warned = false;
    for (;;) {
        uint16_t mv, soc, current;
        battery_status_t next = { .pct = -1 };
        // TI TRM: read voltage, signed average current and relative SOC as LE words.
        if (i2c_master_probe(bus, GAUGE_ADDRESS, 40) == ESP_OK &&
            read_word(gauge, VOLTAGE, &mv) && read_word(gauge, AVERAGE_CURRENT, &current) &&
            read_word(gauge, RELATIVE_SOC, &soc) && soc <= 100 && mv > 0 && mv < 6000) {
            next = (battery_status_t){ .pct = soc, .millivolts = mv, .charging = (int16_t)current > 0 };
        }
        portENTER_CRITICAL(&status_lock);
        status = next;
        portEXIT_CRITICAL(&status_lock);
        if (next.pct < 0 && !warned) {
            ESP_LOGW(TAG, "BQ27220 unavailable");
        }
        warned = next.pct < 0;
        vTaskDelay(pdMS_TO_TICKS(30000));
    }
}

esp_err_t power_start(void)
{
    esp_err_t err = bsp_i2c_init();
    if (err != ESP_OK) {
        return err;
    }
    return xTaskCreate(power_task, "power", 3072, NULL, 3, NULL) == pdPASS ? ESP_OK : ESP_ERR_NO_MEM;
}
#endif
