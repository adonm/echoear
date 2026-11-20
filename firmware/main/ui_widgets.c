#include "ui_widgets.h"

#include <stdlib.h>
#include <string.h>

#include "cat_face.h"

static void style_flat_obj(lv_obj_t *obj, int x, int y, int w, int h)
{
    lv_obj_remove_style_all(obj);
    lv_obj_set_scrollable(obj, false);
    lv_obj_set_clickable(obj, false);
    lv_obj_set_size(obj, w, h);
    lv_obj_set_pos(obj, x, y);
}

lv_obj_t *ui_box(lv_obj_t *parent, int x, int y, int w, int h)
{
    lv_obj_t *obj = parent ? lv_obj_create(parent) : NULL;
    if (obj) {
        style_flat_obj(obj, x, y, w, h);
    }
    return obj;
}

void ui_ink_border(lv_obj_t *obj)
{
    lv_obj_set_style_border_width(obj, 3, 0);
    lv_obj_set_style_border_color(obj, lv_color_hex(CAT_INK), 0);
}

static void style_comic_pill(lv_obj_t *obj, uint32_t color)
{
    lv_obj_set_style_radius(obj, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(color), 0);
    lv_obj_set_style_bg_opa(obj, LV_OPA_COVER, 0);
    ui_ink_border(obj);
}

lv_obj_t *ui_comic_box(lv_obj_t *parent, int x, int y, int w, int h, uint32_t color)
{
    lv_obj_t *obj = ui_box(parent, x, y, w, h);
    if (obj) {
        style_comic_pill(obj, color);
    }
    return obj;
}

lv_obj_t *ui_label(lv_obj_t *parent, const lv_font_t *font, uint32_t color)
{
    lv_obj_t *obj = parent ? lv_label_create(parent) : NULL;
    if (obj) {
        lv_obj_set_style_text_font(obj, font, 0);
        lv_obj_set_style_text_color(obj, lv_color_hex(color), 0);
    }
    return obj;
}

void ui_fit_label(lv_obj_t *label, int w, int h)
{
    lv_obj_set_size(label, w, h);
    lv_obj_set_style_text_align(label, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(label, LV_LABEL_LONG_MODE_DOTS);
}

int ui_utf8_prefix(const char *text, size_t max)
{
    size_t len = strnlen(text, max + 1);
    if (len > max) {
        len = max;
        while (len && ((unsigned char)text[len] & 0xc0) == 0x80) {
            --len;
        }
    }
    return len;
}

const char *ui_field_text(const cJSON *item, const char *key)
{
    const char *text = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(item, key));
    return text ? text : "";
}

uint32_t ui_field_color(const cJSON *item, const char *key, uint32_t fallback)
{
    const char *text = ui_field_text(item, key);
    return text[0] == '#' && strspn(text + 1, "0123456789abcdefABCDEF") == 6 && !text[7]
           ? (uint32_t)strtoul(text + 1, NULL, 16) : fallback;
}

static int field_num(const cJSON *obj, const char *key, int min, int max, int fallback)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsNumber(value) && value->valuedouble >= min && value->valuedouble <= max
           ? (int)value->valuedouble : fallback;
}

static int coordinate(const cJSON *obj, const char *key)
{
    return field_num(obj, key, 0, SCREEN_SIZE - 1, -1);
}

static lv_obj_t *draw_line(lv_obj_t *parent, const cJSON *item, unsigned index, lv_color_t color, int stroke)
{
    static lv_point_precise_t points[MAX_SHAPES][2]; // LVGL borrows these until the artwork is cleared.
    int x1 = coordinate(item, "x1"), y1 = coordinate(item, "y1");
    int x2 = coordinate(item, "x2"), y2 = coordinate(item, "y2");
    lv_obj_t *line = x1 < 0 || y1 < 0 || x2 < 0 || y2 < 0 ? NULL : lv_line_create(parent);
    if (line) {
        int left = LV_MIN(x1, x2), top = LV_MIN(y1, y2);
        points[index][0] = (lv_point_precise_t){ x1 - left, y1 - top };
        points[index][1] = (lv_point_precise_t){ x2 - left, y2 - top };
        style_flat_obj(line, left, top, abs(x2 - x1) + 1, abs(y2 - y1) + 1);
        lv_line_set_points(line, points[index], 2);
        lv_obj_set_style_line_color(line, color, 0);
        lv_obj_set_style_line_width(line, stroke, 0);
        lv_obj_set_style_line_rounded(line, true, 0);
    }
    return line;
}

static lv_obj_t *draw_arc(lv_obj_t *parent, const cJSON *item, int x, int y, int r, lv_color_t color, int stroke)
{
    lv_obj_t *arc = r ? lv_arc_create(parent) : NULL;
    if (arc) {
        style_flat_obj(arc, x - r, y - r, 2 * r, 2 * r);
        lv_arc_set_rotation(arc, 0);
        lv_arc_set_bg_angles(arc, 0, 360);
        lv_arc_set_angles(arc, field_num(item, "a1", 0, 360, 0), field_num(item, "a2", 0, 360, 360));
        lv_obj_set_style_arc_opa(arc, LV_OPA_TRANSP, LV_PART_MAIN);
        lv_obj_set_style_arc_opa(arc, LV_OPA_COVER, LV_PART_INDICATOR);
        lv_obj_set_style_arc_color(arc, color, LV_PART_INDICATOR);
        lv_obj_set_style_arc_width(arc, LV_MIN(stroke, r), LV_PART_INDICATOR);
        lv_obj_set_style_arc_rounded(arc, true, LV_PART_INDICATOR);
    }
    return arc;
}

lv_obj_t *ui_draw_shape(lv_obj_t *parent, const cJSON *shape, unsigned index)
{
    const char *type = ui_field_text(shape, "t");
    uint32_t hex = ui_field_color(shape, "c", 0xf8fafc);
    lv_color_t color = lv_color_hex(hex);
    int stroke = field_num(shape, "w", 1, 24, 3);
    if (!strcmp(type, "line")) {
        return draw_line(parent, shape, index, color, stroke);
    }
    int x = coordinate(shape, "x"), y = coordinate(shape, "y");
    if (x < 0 || y < 0) {
        return NULL;
    }
    const char *value = ui_field_text(shape, "s");
    if (!strcmp(type, "text")) {
        lv_obj_t *text = ui_label(parent, &lv_font_montserrat_16, hex);
        if (text) {
            lv_obj_set_scrollable(text, false);
            lv_obj_set_clickable(text, false);
            lv_label_set_text_fmt(text, "%.*s", ui_utf8_prefix(value, 144), value);
            lv_obj_align(text, LV_ALIGN_CENTER, x - SCREEN_SIZE / 2, y - SCREEN_SIZE / 2);
        }
        return text;
    }
    int r = field_num(shape, "r", 1, SCREEN_SIZE / 2, 0);
    if (!strcmp(type, "arc")) {
        return draw_arc(parent, shape, x, y, r, color, stroke);
    }
    bool circle = !strcmp(type, "circle"), button = !strcmp(type, "btn");
    if (!circle && !button && strcmp(type, "rect")) {
        return NULL;
    }
    int w = circle ? 2 * r : field_num(shape, "w", 1, SCREEN_SIZE, 0);
    int h = circle ? 2 * r : field_num(shape, "h", 1, SCREEN_SIZE, 0);
    lv_obj_t *obj = w && h ? ui_box(parent, circle ? x - r : x, circle ? y - r : y, w, h) : NULL;
    if (!obj) {
        return NULL;
    }
    lv_obj_set_style_radius(obj, circle ? LV_RADIUS_CIRCLE : button ? field_num(shape, "r", 0, 180, 14) : r, 0);
    if (button || cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(shape, "fill"))) {
        lv_obj_set_style_bg_color(obj, color, 0);
        lv_obj_set_style_bg_opa(obj, LV_OPA_COVER, 0);
    } else {
        lv_obj_set_style_border_color(obj, color, 0);
        lv_obj_set_style_border_width(obj, circle ? stroke : 2, 0);
    }
    if (button) {
        lv_obj_t *text = ui_label(obj, &lv_font_montserrat_16, lv_color_brightness(color) > 140 ? 0x251c3b : 0xfffaff);
        if (!text) {
            lv_obj_delete(obj);
            return NULL;
        }
        ui_fit_label(text, w > 8 ? w - 8 : w, LV_MIN(h, 20));
        lv_label_set_text_fmt(text, "%.*s", ui_utf8_prefix(value, 144), value);
        lv_obj_center(text);
    }
    return obj;
}
