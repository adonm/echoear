#pragma once

#include "lvgl.h"
#include "ui.h"

enum { CAT_INK = 0x1a1926 };

bool cat_face_init(lv_obj_t *parent, const uint8_t mac[6]);
void cat_face_set_phase(ui_phase_t phase);
void cat_face_set_sleeping(bool asleep);
void cat_face_set_battery(int pct, bool charging);
const char *cat_face_name(void);
uint32_t cat_face_accent(void);
