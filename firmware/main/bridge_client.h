#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

#include "esp_err.h"

enum { BRIDGE_TARGET_CAP = 16 };

esp_err_t bridge_client_start_registration(void);
void bridge_client_network_changed(bool connected);
void bridge_client_get_peer(char peer[BRIDGE_TARGET_CAP]);
bool bridge_client_valid_target(const char *target);

// Blocking bridge operations, used only by the conversation worker.
esp_err_t bridge_voice(const int16_t *pcm, size_t bytes, char *error, size_t error_len);
esp_err_t bridge_client_ask_text(const char *question);
esp_err_t bridge_client_send_intercom(const char *target, const int16_t *pcm, size_t sample_count);
esp_err_t bridge_reset(char *error, size_t error_len);
