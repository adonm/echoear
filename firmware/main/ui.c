#include "ui.h"

#include <string.h>

#include "bsp/esp_vocat.h"
#include "esp_log.h"

enum { SCREEN_SIZE = 360, MAX_SHAPES = 24 };

static lv_obj_t *network_label;
static lv_obj_t *connection_dot;
static lv_obj_t *scroll;
static lv_obj_t *message_label;
static lv_obj_t *talk;
static lv_obj_t *talk_label;
static lv_obj_t *reset;
static lv_obj_t *diagram;
static lv_obj_t *artwork;
static lv_obj_t *diagram_title;
static lv_obj_t *diagram_subtitle;
static lv_obj_t *diagram_talk;
static lv_obj_t *diagram_talk_label;
static lv_obj_t *diagram_back;
static lv_point_precise_t line_points[MAX_SHAPES][2];
static ui_action_cb_t dispatch;
static ui_phase_t phase;
static bool online;
static bool audio_ready;

static void update_talk(lv_obj_t *button, lv_obj_t *text, bool recording, bool enabled)
{
    lv_label_set_text(text, recording ? "Tap to Send" : "Tap to Talk");
    lv_obj_set_style_bg_color(button, lv_color_hex(recording ? 0x9b3045 : 0x075d62), LV_PART_MAIN);
    if (enabled) {
        lv_obj_remove_state(button, LV_STATE_DISABLED);
    } else {
        lv_obj_add_state(button, LV_STATE_DISABLED);
    }
}

static void update_controls(void)
{
    const bool recording = phase == UI_RECORDING;
    const bool talk_enabled = recording || (phase == UI_IDLE && online && audio_ready);
    update_talk(talk, talk_label, recording, talk_enabled);
    update_talk(diagram_talk, diagram_talk_label, recording, talk_enabled);
    if (phase == UI_IDLE && online) {
        lv_obj_remove_state(reset, LV_STATE_DISABLED);
    } else {
        lv_obj_add_state(reset, LV_STATE_DISABLED);
    }
}

static void set_phase(ui_phase_t next, const char *message)
{
    phase = next;
    if (message) {
        lv_label_set_text(message_label, message);
        lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    }
    update_controls();
}

static void hide_screen(void)
{
    lv_obj_set_hidden(diagram, true);
    lv_obj_clean(artwork);
    lv_label_set_text(diagram_title, "");
    lv_label_set_text(diagram_subtitle, "");
}

static void on_click(lv_event_t *event)
{
    lv_obj_t *button = lv_event_get_target(event);
    if (button == diagram_back) {
        hide_screen();
    } else if (button == reset) {
        if (dispatch(UI_NEW_CHAT)) {
            set_phase(UI_BUSY, "Starting a new chat…");
        }
    } else if (phase == UI_RECORDING) {
        if (dispatch(UI_STOP_RECORDING)) {
            if (button == diagram_talk) {
                hide_screen();
            }
            set_phase(UI_BUSY, "Sending…");
        }
    } else if (phase == UI_IDLE && dispatch(UI_START_RECORDING)) {
        if (button == diagram_talk) {
            hide_screen();
        }
        set_phase(UI_RECORDING, "Listening…\n\nTap again to send, or wait for the 15-second limit.");
    }
}

static lv_obj_t *label(lv_obj_t *parent, const lv_font_t *font, uint32_t color)
{
    lv_obj_t *obj = lv_label_create(parent);
    lv_obj_set_style_text_font(obj, font, 0);
    lv_obj_set_style_text_color(obj, lv_color_hex(color), 0);
    return obj;
}

static lv_obj_t *pill(lv_obj_t *parent, int width, int height, int y, uint32_t color)
{
    lv_obj_t *obj = lv_button_create(parent);
    lv_obj_set_size(obj, width, height);
    lv_obj_align(obj, LV_ALIGN_TOP_MID, 0, y);
    lv_obj_set_style_radius(obj, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(color), 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(0x252b34), LV_STATE_DISABLED);
    lv_obj_add_event_cb(obj, on_click, LV_EVENT_CLICKED, NULL);
    return obj;
}

static void decorative(lv_obj_t *obj)
{
    lv_obj_set_scrollable(obj, false);
    lv_obj_set_clickable(obj, false);
}

static lv_obj_t *plain_box(lv_obj_t *parent, int x, int y, int width, int height)
{
    lv_obj_t *obj = lv_obj_create(parent);
    lv_obj_remove_style_all(obj);
    decorative(obj);
    lv_obj_set_size(obj, width, height);
    lv_obj_set_pos(obj, x, y);
    return obj;
}

static lv_obj_t *diagram_heading(const lv_font_t *font, int y, int width)
{
    lv_obj_t *obj = label(diagram, font, 0xf8fafc);
    lv_obj_set_width(obj, width);
    lv_obj_set_style_text_align(obj, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(obj, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_align(obj, LV_ALIGN_TOP_MID, 0, y);
    return obj;
}

static void init_diagram(lv_obj_t *screen)
{
    diagram = plain_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    lv_obj_set_clickable(diagram, true);
    lv_obj_set_style_bg_opa(diagram, LV_OPA_COVER, 0);
    artwork = plain_box(diagram, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    diagram_title = diagram_heading(&lv_font_montserrat_24, 26, 180);
    diagram_subtitle = diagram_heading(&lv_font_montserrat_16, 262, 212);

    diagram_talk = pill(diagram, 134, 38, 285, 0x075d62);
    diagram_talk_label = label(diagram_talk, &lv_font_montserrat_16, 0xf1f6fb);
    lv_obj_center(diagram_talk_label);
    diagram_back = pill(diagram, 65, 27, 325, 0x293a51);
    lv_obj_t *back_label = label(diagram_back, &lv_font_montserrat_16, 0xe1ebf4);
    lv_label_set_text(back_label, "Text");
    lv_obj_center(back_label);
    hide_screen();
}

static const char *field_text(const cJSON *obj, const char *key)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsString(value) ? value->valuestring : "";
}

static int field_num(const cJSON *obj, const char *key, int min, int max, int fallback)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsNumber(value) && value->valuedouble >= min && value->valuedouble <= max
           ? (int)value->valuedouble : fallback;
}

static lv_color_t field_color(const cJSON *obj, const char *key, uint32_t fallback)
{
    const char *text = field_text(obj, key);
    if (text[0] != '#' || strlen(text) != 7) {
        return lv_color_hex(fallback);
    }
    uint32_t color = 0;
    for (int i = 1; i <= 6; ++i) {
        int digit = text[i] >= '0' && text[i] <= '9' ? text[i] - '0'
                  : text[i] >= 'a' && text[i] <= 'f' ? text[i] - 'a' + 10
                  : text[i] >= 'A' && text[i] <= 'F' ? text[i] - 'A' + 10 : -1;
        if (digit < 0) {
            return lv_color_hex(fallback);
        }
        color = (color << 4) | digit;
    }
    return lv_color_hex(color);
}

static void draw_shape(const cJSON *shape, unsigned index)
{
    const char *type = field_text(shape, "t");
    lv_color_t color = field_color(shape, "c", 0xf8fafc);
    int x = field_num(shape, "x", 0, SCREEN_SIZE - 1, -1);
    int y = field_num(shape, "y", 0, SCREEN_SIZE - 1, -1);
    int radius = field_num(shape, "r", 1, SCREEN_SIZE / 2, 0);
    int stroke = field_num(shape, "w", 1, 24, 3);
    if (strcmp(type, "line") == 0) {
        int x1 = field_num(shape, "x1", 0, SCREEN_SIZE - 1, -1);
        int y1 = field_num(shape, "y1", 0, SCREEN_SIZE - 1, -1);
        int x2 = field_num(shape, "x2", 0, SCREEN_SIZE - 1, -1);
        int y2 = field_num(shape, "y2", 0, SCREEN_SIZE - 1, -1);
        if (x1 < 0 || y1 < 0 || x2 < 0 || y2 < 0) {
            return;
        }
        lv_point_precise_t *points = line_points[index];
        points[0] = (lv_point_precise_t){ x1, y1 };
        points[1] = (lv_point_precise_t){ x2, y2 };
        lv_obj_t *line = lv_line_create(artwork);
        lv_obj_remove_style_all(line);
        decorative(line);
        lv_obj_set_size(line, SCREEN_SIZE, SCREEN_SIZE);
        lv_line_set_points(line, points, 2);
        lv_obj_set_style_line_color(line, color, 0);
        lv_obj_set_style_line_width(line, stroke, 0);
        lv_obj_set_style_line_rounded(line, true, 0);
        return;
    }
    if (x < 0 || y < 0) {
        return;
    }
    if (strcmp(type, "text") == 0) {
        lv_obj_t *text = label(artwork, &lv_font_montserrat_16, 0xf8fafc);
        decorative(text);
        lv_label_set_text(text, field_text(shape, "s"));
        lv_obj_set_style_text_color(text, color, 0);
        lv_obj_align(text, LV_ALIGN_CENTER, x - SCREEN_SIZE / 2, y - SCREEN_SIZE / 2);
    } else if (strcmp(type, "arc") == 0 && radius) {
        lv_obj_t *arc = lv_arc_create(artwork);
        lv_obj_remove_style_all(arc);
        decorative(arc);
        lv_obj_set_size(arc, 2 * radius, 2 * radius);
        lv_obj_set_pos(arc, x - radius, y - radius);
        lv_arc_set_rotation(arc, 0);
        lv_arc_set_bg_angles(arc, 0, 360);
        lv_arc_set_angles(arc, field_num(shape, "a1", 0, 360, 0), field_num(shape, "a2", 0, 360, 360));
        lv_obj_set_style_arc_opa(arc, LV_OPA_TRANSP, LV_PART_MAIN);
        lv_obj_set_style_arc_opa(arc, LV_OPA_COVER, LV_PART_INDICATOR);
        lv_obj_set_style_arc_color(arc, color, LV_PART_INDICATOR);
        lv_obj_set_style_arc_width(arc, stroke < radius ? stroke : radius, LV_PART_INDICATOR);
        lv_obj_set_style_arc_rounded(arc, true, LV_PART_INDICATOR);
    } else if (strcmp(type, "circle") == 0 || strcmp(type, "rect") == 0) {
        bool circle = type[0] == 'c';
        int width = circle ? 2 * radius : field_num(shape, "w", 1, SCREEN_SIZE, 0);
        int height = circle ? 2 * radius : field_num(shape, "h", 1, SCREEN_SIZE, 0);
        if (!width || !height) {
            return;
        }
        lv_obj_t *obj = plain_box(artwork, circle ? x - radius : x, circle ? y - radius : y, width, height);
        lv_obj_set_style_radius(obj, circle ? LV_RADIUS_CIRCLE : radius, 0);
        if (cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(shape, "fill"))) {
            lv_obj_set_style_bg_color(obj, color, 0);
            lv_obj_set_style_bg_opa(obj, LV_OPA_COVER, 0);
        } else {
            lv_obj_set_style_border_color(obj, color, 0);
            lv_obj_set_style_border_width(obj, circle ? stroke : 2, 0);
        }
    }
}

static esp_err_t show_screen(const cJSON *screen)
{
    const cJSON *shapes = cJSON_GetObjectItemCaseSensitive(screen, "shapes");
    if (!cJSON_IsObject(screen) || (shapes && (!cJSON_IsArray(shapes) ||
                                              cJSON_GetArraySize(shapes) > MAX_SHAPES))) {
        return ESP_ERR_INVALID_ARG;
    }
    hide_screen();
    lv_obj_set_style_bg_color(diagram, field_color(screen, "bg", 0x0b1020), 0);
    lv_label_set_text(diagram_title, field_text(screen, "title"));
    lv_label_set_text(diagram_subtitle, field_text(screen, "subtitle"));
    // Line widgets borrow their points; the old widgets were deleted before reusing these slots.
    unsigned index = 0;
    const cJSON *shape;
    cJSON_ArrayForEach(shape, shapes) {
        draw_shape(shape, index++);
    }
    lv_obj_set_hidden(diagram, false);
    return ESP_OK;
}

esp_err_t ui_init(const char *device_suffix, ui_action_cb_t action_cb)
{
    lv_display_t *display = bsp_display_start();
    if (!display || !bsp_display_get_input_dev() || !bsp_display_lock(0)) {
        return ESP_FAIL;
    }
    dispatch = action_cb;
    lv_obj_t *screen = lv_display_get_screen_active(display);
    lv_obj_set_style_bg_color(screen, lv_color_hex(0x0e131b), 0);
    lv_obj_set_scrollable(screen, false);

    lv_obj_t *title = label(screen, &lv_font_montserrat_24, 0xf1f6fb);
    lv_label_set_text_fmt(title, "EchoEar-%s", device_suffix);
    lv_obj_align(title, LV_ALIGN_TOP_MID, 0, 31);
    network_label = label(screen, &lv_font_montserrat_16, 0xa5bdd2);
    lv_label_set_text(network_label, "Connecting...");
    lv_obj_align(network_label, LV_ALIGN_TOP_MID, 7, 61);
    connection_dot = lv_obj_create(screen);
    lv_obj_remove_style_all(connection_dot);
    lv_obj_set_clickable(connection_dot, false);
    lv_obj_set_scrollable(connection_dot, false);
    lv_obj_set_size(connection_dot, 7, 7);
    lv_obj_set_style_radius(connection_dot, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_opa(connection_dot, LV_OPA_COVER, 0);
    lv_obj_set_style_bg_color(connection_dot, lv_color_hex(0xe8b85f), 0);
    lv_obj_align_to(connection_dot, network_label, LV_ALIGN_OUT_LEFT_MID, -7, 0);

    scroll = lv_obj_create(screen);
    lv_obj_set_size(scroll, 232, 158);
    lv_obj_align(scroll, LV_ALIGN_TOP_MID, 0, 90);
    lv_obj_set_style_bg_color(scroll, lv_color_hex(0x18212b), 0);
    lv_obj_set_style_border_width(scroll, 0, 0);
    lv_obj_set_style_radius(scroll, 18, 0);
    lv_obj_set_style_pad_all(scroll, 13, 0);
    lv_obj_set_scroll_dir(scroll, LV_DIR_VER);
    lv_obj_set_scrollbar_mode(scroll, LV_SCROLLBAR_MODE_AUTO);
    message_label = label(scroll, &lv_font_montserrat_16, 0xe2edf6);
    lv_obj_set_width(message_label, lv_pct(100));
    lv_label_set_long_mode(message_label, LV_LABEL_LONG_WRAP);

    talk = pill(screen, 176, 49, 260, 0x075d62);
    talk_label = label(talk, &lv_font_montserrat_20, 0xf1f6fb);
    lv_obj_center(talk_label);
    reset = pill(screen, 98, 30, 315, 0x293a51);
    lv_obj_t *reset_label = label(reset, &lv_font_montserrat_16, 0xe1ebf4);
    lv_label_set_text(reset_label, "New Chat");
    lv_obj_center(reset_label);
    init_diagram(screen);
    set_phase(UI_IDLE, "Starting…");
    bsp_display_unlock();

    esp_err_t err = bsp_display_backlight_on();
    if (err == ESP_OK) {
        ESP_LOGI("ui", "Display and touch initialized (%dx%d)", BSP_LCD_H_RES, BSP_LCD_V_RES);
    }
    return err;
}

void ui_set_network(bool connected, const char *ip)
{
    bsp_display_lock(0);
    online = connected;
    lv_label_set_text(network_label, connected ? ip : "Connecting...");
    lv_obj_update_layout(network_label);
    lv_obj_align_to(connection_dot, network_label, LV_ALIGN_OUT_LEFT_MID, -7, 0);
    lv_obj_set_style_bg_color(connection_dot, lv_color_hex(connected ? 0x59e3ac : 0xe8b85f), 0);
    update_controls();
    bsp_display_unlock();
}

void ui_set_audio_ready(bool ready)
{
    bsp_display_lock(0);
    audio_ready = ready;
    set_phase(UI_IDLE, ready ? "Ready" : "Audio initialization failed");
    bsp_display_unlock();
}

void ui_set_phase(ui_phase_t next, const char *message)
{
    bsp_display_lock(0);
    set_phase(next, message);
    bsp_display_unlock();
}

void ui_show_reply(const char *transcript, const char *reply, const cJSON *screen)
{
    bsp_display_lock(0);
    lv_label_set_text_fmt(message_label, "You: %s\n\nAI: %s", transcript, reply);
    lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    set_phase(UI_IDLE, NULL);
    if (show_screen(screen) != ESP_OK) {
        hide_screen();
    }
    bsp_display_unlock();
}

esp_err_t ui_show_screen(const cJSON *screen_json)
{
    bsp_display_lock(0);
    esp_err_t err = show_screen(screen_json);
    bsp_display_unlock();
    return err;
}

void ui_hide_screen(void)
{
    bsp_display_lock(0);
    hide_screen();
    bsp_display_unlock();
}
