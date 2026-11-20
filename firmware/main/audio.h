#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

enum { AUDIO_RATE = 16000, AUDIO_MAX_SECONDS = 15 };
typedef enum { AUDIO_START_TONE, AUDIO_THINK_TONE, AUDIO_QUIET_TONE, AUDIO_HAPPY_TONE } audio_tone_t;

esp_err_t audio_init(void);
void audio_tone(audio_tone_t tone);
esp_err_t audio_record(bool (*stop_requested)(void), const int16_t **pcm, size_t *bytes);
int audio_get_volume(void);
esp_err_t audio_set_volume(int level);
void audio_stream_begin(void);
esp_err_t audio_stream_write(const int16_t *pcm, size_t samples);
esp_err_t audio_stream_end(void);
bool audio_playback_stopped(void);
void audio_stop_playback(void);
void audio_request_chime(void);
