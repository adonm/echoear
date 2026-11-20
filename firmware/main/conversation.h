#pragma once

#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

enum { CONVERSATION_QUESTION_MAX = 512 };

// Ownership of the upload transfers only on success.
esp_err_t conversation_submit_speech(int16_t *pcm, size_t samples);
esp_err_t conversation_ask(const char *question);
esp_err_t conversation_start_intercom(const char *target);
void conversation_chime(void);

// Called only by the conversation worker, with borrowed PCM.
esp_err_t conversation_play(const int16_t *pcm, size_t samples);
esp_err_t conversation_stream_begin(const char *caption);
esp_err_t conversation_stream_end(void);
