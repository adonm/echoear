#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

enum { AUDIO_RATE = 16000, AUDIO_MAX_SECONDS = 15 };
typedef enum { AUDIO_START_TONE, AUDIO_STOP_TONE, AUDIO_REPLY_TONE } audio_tone_t;

esp_err_t audio_init(void);
void audio_tone(audio_tone_t tone);
esp_err_t audio_record(bool (*stop_requested)(void), const int16_t **pcm, size_t *bytes);
