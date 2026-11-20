#pragma once

#include <stdbool.h>
#include <stdint.h>

#include "cJSON.h"
#include "esp_err.h"

typedef enum { UI_START_RECORDING, UI_STOP_RECORDING, UI_NEW_CHAT, UI_STOP_SPEAKING } ui_action_t;
typedef enum { UI_IDLE, UI_RECORDING, UI_BUSY, UI_SPEAKING } ui_phase_t;
typedef bool (*ui_action_cb_t)(ui_action_t action);

esp_err_t ui_init(const char *device_suffix, const uint8_t mac[6], ui_action_cb_t action_cb);
void ui_set_network(bool connected, const char *ip);
void ui_set_audio_ready(bool ready);
void ui_set_phase(ui_phase_t phase, const char *message);
void ui_show_reply(const char *transcript, const char *reply, const cJSON *screen);
void ui_clear_reply(void);
bool ui_wake(void);
bool ui_is_sleeping(void);
void ui_pet(void);
void ui_show_volume(int level);
void ui_set_battery(int pct, bool charging);
esp_err_t ui_show_screen(const cJSON *screen_json);
void ui_hide_screen(void);
