#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

// Blocking bridge operations, used only by the conversation worker.
esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, char *error, size_t error_len);
esp_err_t bridge_client_ask_text(const char *question);
esp_err_t bridge_reset(char *error, size_t error_len);
