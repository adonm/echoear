#pragma once

#include "audio.h"

enum { MEDIA_TARGET_CAP = 64 };

esp_err_t media_init(bool audio_ready);
bool media_record_start(const char *target);
void media_record_stop(void);
bool media_tts_start(void);
void media_tts_end(void);
bool media_tts_pcm(const void *data, size_t bytes);
void media_interrupt(void);
void media_link_lost(void);
void media_chime(audio_tone_t tone);
// Ownership of the PCM transfers only on success.
esp_err_t media_submit_speech(int16_t *pcm, size_t samples);
