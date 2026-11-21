#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

esp_err_t ws_client_init(void);
void ws_client_network_changed(bool online);
bool ws_client_connected(void);
uint32_t ws_client_session(void);
void ws_client_pointer(const char *event, int x, int y);
bool ws_client_rec_start(uint32_t session, const char *target);
bool ws_client_rec_pcm(uint32_t session, const int16_t *pcm, size_t samples);
void ws_client_call_pcm(uint32_t session, uint32_t id, const int16_t *pcm, uint8_t prefix);
void ws_client_call_failed(uint32_t session, uint32_t id);
void ws_client_rec_end(uint32_t session, const char *target, bool cancelled);
void ws_client_tts_ack(uint32_t session, uint32_t audio_id, bool aborted);
