#pragma once

#include <stddef.h>
#include <stdint.h>

#include "cJSON.h"
#include "esp_err.h"

typedef struct {
    cJSON *json; // Caller owns this tree; the remaining fields borrow from it.
    const char *transcript;
    const char *reply;
    const cJSON *screen;
} bridge_reply_t;

esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, bridge_reply_t *result, char *error, size_t error_len);
esp_err_t bridge_reset(char *error, size_t error_len);
