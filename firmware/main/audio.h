#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

enum { AUDIO_RATE = 16000, AUDIO_MAX_SECONDS = 60, AUDIO_RECORD_CHUNK_SAMPLES = 1280 };
typedef enum { AUDIO_START_TONE, AUDIO_THINK_TONE, AUDIO_QUIET_TONE, AUDIO_HAPPY_TONE } audio_tone_t;
typedef esp_err_t (*audio_capture_sink_t)(const int16_t *pcm, size_t samples, void *ctx);

esp_err_t audio_init(void);
void audio_tone(audio_tone_t tone);
esp_err_t audio_record_stream(bool (*stop_requested)(void), audio_capture_sink_t sink, void *ctx);
int audio_get_volume(void);
esp_err_t audio_set_volume(int level);
void audio_stream_begin(void);
esp_err_t audio_stream_write(const int16_t *pcm, size_t samples);
esp_err_t audio_stream_end(void);
bool audio_playback_stopped(void);
void audio_stop_playback(void);
void audio_request_chime(void);
