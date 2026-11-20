#pragma once

#include <stdbool.h>

#include "cJSON.h"
#include "esp_err.h"

esp_err_t display_init(void);
void display_refresh(void);
void display_set_link(bool connected);
bool display_apply_scene(const cJSON *msg);
void display_wake(void);
void display_set_brightness(int level);
bool display_is_sleeping(void);
