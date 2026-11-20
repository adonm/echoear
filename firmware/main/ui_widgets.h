#pragma once

#include "cJSON.h"
#include "lvgl.h"

enum { SCREEN_SIZE = 360, MAX_SHAPES = 24 };

lv_obj_t *ui_box(lv_obj_t *parent, int x, int y, int w, int h);
lv_obj_t *ui_comic_box(lv_obj_t *parent, int x, int y, int w, int h, uint32_t color);
void ui_ink_border(lv_obj_t *obj);
lv_obj_t *ui_label(lv_obj_t *parent, const lv_font_t *font, uint32_t color);
void ui_fit_label(lv_obj_t *label, int w, int h);
int ui_utf8_prefix(const char *text, size_t max);
const char *ui_field_text(const cJSON *item, const char *key);
uint32_t ui_field_color(const cJSON *item, const char *key, uint32_t fallback);
lv_obj_t *ui_draw_shape(lv_obj_t *parent, const cJSON *shape, unsigned index);
