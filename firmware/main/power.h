#pragma once

#include <stdbool.h>

#include "esp_err.h"

typedef struct {
    int pct; // -1 when absent or unavailable
    unsigned millivolts;
    bool charging;
} battery_status_t;

battery_status_t battery_status(void);
esp_err_t power_start(void);
