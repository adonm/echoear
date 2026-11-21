#pragma once

#include <stdbool.h>

#include "esp_err.h"

// Used only by the task that owns codec I/O.
bool wake_word_start(void);
void wake_word_stop(void);
void wake_word_reset(void);
esp_err_t wake_word_poll(bool *detected);
