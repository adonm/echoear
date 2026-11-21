#pragma once

#include <stdint.h>

#include "esp_err.h"

esp_err_t ota_server_start(void);
void ota_check_boot(int64_t boot_us);
