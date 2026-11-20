#include "ui.h"

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bridge_client.h"
#include "bsp/esp_vocat.h"
#include "cat_face.h"
#include "conversation.h"
#include "esp_log.h"

enum { SCREEN_SIZE = 360, MAX_SHAPES = 24, CHAT_ACTION_Y = 293 };

static lv_obj_t *network_label;
static lv_obj_t *cat;
static lv_obj_t *chat;
static lv_obj_t *caption;
static lv_obj_t *volume_pill;
static lv_obj_t *volume_label;
static lv_timer_t *volume_timer;
static lv_obj_t *cat_text;
static lv_obj_t *chat_cat;
static lv_obj_t *chat_new;
static lv_obj_t *chat_peer;
static lv_obj_t *chat_peer_label;
static lv_obj_t *scroll;
static lv_obj_t *message_label;
static lv_obj_t *talk;
static lv_obj_t *talk_label;
static lv_obj_t *reset;
static lv_obj_t *diagram;
static lv_obj_t *artwork;
static lv_obj_t *diagram_title;
static lv_obj_t *diagram_subtitle;
static lv_obj_t *diagram_info;
static lv_obj_t *diagram_talk;
static lv_obj_t *diagram_talk_label;
static lv_obj_t *diagram_back;
static lv_obj_t *highlighted;
static lv_timer_t *highlight_timer;
static lv_timer_t *notice_timer;
static lv_point_precise_t line_points[MAX_SHAPES][2];
typedef struct { char *tap; char *ask; char *reply_to; } shape_action_t;
static shape_action_t shape_actions[MAX_SHAPES];
static char displayed_peer[BRIDGE_TARGET_CAP];
static ui_action_cb_t dispatch;
static ui_phase_t phase;
static bool online;
static bool audio_ready;
static bool text_view;
static bool diagram_asking;
static bool touch_woke;
static atomic_bool sleeping;
static uint32_t last_activity;

static bool wake_locked(void)
{
    last_activity = lv_tick_get();
    if (!atomic_exchange(&sleeping, false)) {
        return false;
    }
    bsp_display_brightness_set(85);
    cat_face_set_sleeping(false);
    return true;
}

bool ui_wake(void)
{
    bsp_display_lock(0);
    bool woke = wake_locked();
    bsp_display_unlock();
    return woke;
}

bool ui_is_sleeping(void)
{
    return atomic_load(&sleeping);
}

static void touch_started(lv_event_t *event)
{
    touch_woke = wake_locked();
}

static void set_enabled(lv_obj_t *button, bool enabled)
{
    if (enabled) {
        lv_obj_remove_state(button, LV_STATE_DISABLED);
    } else {
        lv_obj_add_state(button, LV_STATE_DISABLED);
    }
}

static void update_talk(lv_obj_t *button, lv_obj_t *text, bool enabled)
{
    const char *title = phase == UI_RECORDING ? "Send" : phase == UI_SPEAKING ? "Stop speech" : "Tap to Talk";
    lv_label_set_text(text, title);
    lv_obj_set_style_bg_color(button, lv_color_hex(phase == UI_RECORDING ? 0x9b3045 : 0x5444a3), LV_PART_MAIN);
    set_enabled(button, enabled);
}

static bool can_intercom(void)
{
    return online && audio_ready && (phase == UI_IDLE || phase == UI_SPEAKING);
}

static void update_controls(void)
{
    const bool talk_enabled = phase == UI_RECORDING || phase == UI_SPEAKING ||
                              (phase == UI_IDLE && online && audio_ready);
    update_talk(talk, talk_label, talk_enabled);
    update_talk(diagram_talk, diagram_talk_label, talk_enabled);
    set_enabled(reset, phase == UI_IDLE && online);
    set_enabled(chat_new, phase == UI_IDLE && online);
    set_enabled(chat_peer, can_intercom());
}

static int utf8_prefix(const char *text, size_t max)
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

static void set_diagram_subtitle(const char *message)
{
    lv_label_set_text_fmt(diagram_subtitle, "%.*s", utf8_prefix(message, 256), message);
    lv_obj_set_hidden(diagram_info, !message[0]);
}

static void set_phase(ui_phase_t next, const char *message)
{
    lv_timer_pause(notice_timer);
    if (phase != next) {
        wake_locked();
    }
    phase = next;
    char greeting[24];
    if (!message) {
        switch (phase) {
        case UI_IDLE:
            if (online && audio_ready) {
                snprintf(greeting, sizeof(greeting), "Tap %s!", cat_face_name());
                message = greeting;
            } else {
                message = audio_ready ? "Joining Wi-Fi..." : "Sound unavailable";
            }
            break;
        case UI_RECORDING: message = "Listening!"; break;
        case UI_BUSY: message = "Hmm..."; break;
        case UI_SPEAKING: message = "Meow!"; break;
        }
    }
    lv_label_set_text(caption, message);
    if (phase == UI_IDLE && diagram_asking) {
        set_diagram_subtitle(message);
        diagram_asking = false;
    }
    cat_face_set_phase(phase);
    update_controls();
}

static void restore_caption(lv_timer_t *timer)
{
    set_phase(UI_IDLE, NULL);
}

static void update_pages(void)
{
    bool diagram_visible = !lv_obj_is_hidden(diagram);
    lv_obj_set_hidden(cat, diagram_visible || text_view);
    lv_obj_set_hidden(chat, diagram_visible || !text_view);
}

static void idle_check(lv_timer_t *timer)
{
    if (phase != UI_IDLE || !lv_obj_is_hidden(diagram) || ui_is_sleeping() ||
        lv_tick_get() - last_activity < 120000) {
        return;
    }
    if (bsp_display_brightness_set(12) == ESP_OK) {
        atomic_store(&sleeping, true);
        text_view = false;
        update_pages();
        cat_face_set_sleeping(true);
    }
}

static void clear_highlight(lv_timer_t *timer)
{
    if (highlighted) {
        lv_obj_remove_state(highlighted, LV_STATE_USER_1);
        highlighted = NULL;
    }
    lv_timer_pause(timer);
}

static void hide_screen(void)
{
    clear_highlight(highlight_timer);
    lv_obj_set_hidden(diagram, true);
    lv_obj_clean(artwork);
    for (unsigned i = 0; i < MAX_SHAPES; ++i) {
        free(shape_actions[i].tap);
        free(shape_actions[i].ask);
        free(shape_actions[i].reply_to);
        shape_actions[i] = (shape_action_t){ 0 };
    }
    diagram_asking = false;
    lv_label_set_text(diagram_title, "");
    set_diagram_subtitle("");
    update_pages();
}

static bool start_intercom(const char *target)
{
    if (!can_intercom() || conversation_start_intercom(target) != ESP_OK) {
        return false;
    }
    char message[sizeof("To !") + BRIDGE_TARGET_CAP];
    snprintf(message, sizeof(message), "To %s!", target);
    wake_locked();
    text_view = false;
    hide_screen();
    set_phase(UI_RECORDING, message);
    return true;
}

static void talk_action(bool dismiss_diagram)
{
    if (phase == UI_SPEAKING) {
        dispatch(UI_STOP_SPEAKING);
    } else if (phase == UI_RECORDING) {
        if (dispatch(UI_STOP_RECORDING)) {
            if (dismiss_diagram) {
                text_view = false;
                hide_screen();
            }
            set_phase(UI_BUSY, NULL);
        }
    } else if (phase == UI_IDLE && online && audio_ready && dispatch(UI_START_RECORDING)) {
        if (dismiss_diagram) {
            text_view = false;
            hide_screen();
        }
        set_phase(UI_RECORDING, NULL);
    }
}

void ui_pet(void)
{
    bsp_display_lock(0);
    if (!wake_locked()) {
        talk_action(false);
    }
    bsp_display_unlock();
}

static void on_click(lv_event_t *event)
{
    lv_obj_t *button = lv_event_get_target(event);
    if (button == diagram_back) {
        text_view = false;
        hide_screen();
    } else if (button == cat_text || button == chat_cat) {
        text_view = button == cat_text;
        update_pages();
    } else if (button == chat_peer) {
        start_intercom(displayed_peer);
    } else if (button == reset || button == chat_new) {
        if (dispatch(UI_NEW_CHAT)) {
            set_phase(UI_BUSY, "Freshening up...");
        }
    } else if (button != cat || !touch_woke) {
        talk_action(button == diagram_talk);
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
    lv_obj_set_style_pad_all(obj, 0, 0);
    lv_obj_set_style_shadow_width(obj, 0, 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(color), 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(0x252b34), LV_STATE_DISABLED);
    lv_obj_add_event_cb(obj, on_click, LV_EVENT_CLICKED, NULL);
    return obj;
}

static lv_obj_t *named_pill(lv_obj_t *parent, const char *title, int x, int y, uint32_t color)
{
    lv_obj_t *obj = pill(parent, 68, 30, y, color);
    lv_obj_align(obj, LV_ALIGN_TOP_MID, x, y);
    lv_obj_set_style_border_width(obj, 3, 0);
    lv_obj_set_style_border_color(obj, lv_color_hex(CAT_INK), 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(0xd7ccc9), LV_STATE_DISABLED);
    lv_obj_set_style_bg_color(obj, lv_color_hex(0xffcb9a), LV_STATE_PRESSED);
    lv_obj_t *text = label(obj, &lv_font_montserrat_16, CAT_INK);
    lv_label_set_text(text, title);
    lv_obj_center(text);
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

static void hide_volume(lv_timer_t *timer)
{
    lv_obj_set_hidden(volume_pill, true);
    lv_timer_pause(timer);
}

static void init_volume(lv_obj_t *screen)
{
    volume_pill = plain_box(screen, 103, 214, 154, 34);
    lv_obj_set_style_radius(volume_pill, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(volume_pill, lv_color_hex(cat_face_accent()), 0);
    lv_obj_set_style_bg_opa(volume_pill, LV_OPA_COVER, 0);
    lv_obj_set_style_border_width(volume_pill, 3, 0);
    lv_obj_set_style_border_color(volume_pill, lv_color_hex(CAT_INK), 0);
    volume_label = label(volume_pill, &lv_font_montserrat_16, CAT_INK);
    lv_obj_center(volume_label);
    volume_timer = lv_timer_create(hide_volume, 1500, NULL);
    hide_volume(volume_timer);
}

void ui_show_volume(int level)
{
    bsp_display_lock(0);
    wake_locked();
    lv_label_set_text_fmt(volume_label, "Volume %d%%", level);
    lv_obj_set_hidden(volume_pill, false);
    lv_timer_reset(volume_timer);
    lv_timer_resume(volume_timer);
    bsp_display_unlock();
}

void ui_set_battery(int pct, bool charging)
{
    bsp_display_lock(0);
    cat_face_set_battery(pct, charging);
    bsp_display_unlock();
}

static lv_obj_t *diagram_heading(const lv_font_t *font, int y, int width)
{
    lv_obj_t *obj = label(diagram, font, 0xf8fafc);
    lv_obj_set_size(obj, width, 29);
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
    diagram_info = plain_box(diagram, 65, 247, 230, 36);
    lv_obj_set_style_radius(diagram_info, 15, 0);
    lv_obj_set_style_bg_color(diagram_info, lv_color_hex(0x1b1630), 0);
    lv_obj_set_style_bg_opa(diagram_info, LV_OPA_90, 0);
    diagram_subtitle = label(diagram_info, &lv_font_montserrat_16, 0xfff3fd);
    lv_obj_set_size(diagram_subtitle, 216, 34);
    lv_obj_set_style_text_align(diagram_subtitle, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(diagram_subtitle, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_center(diagram_subtitle);

    diagram_talk = pill(diagram, 142, 36, 286, 0x5444a3);
    diagram_talk_label = label(diagram_talk, &lv_font_montserrat_16, 0xf1f6fb);
    lv_obj_center(diagram_talk_label);
    diagram_back = named_pill(diagram, "Cat", 0, 324, 0xfffaee);
    highlight_timer = lv_timer_create(clear_highlight, 200, NULL);
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

static lv_obj_t *draw_shape(const cJSON *shape, unsigned index)
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
            return NULL;
        }
        int left = LV_MIN(x1, x2);
        int top = LV_MIN(y1, y2);
        lv_point_precise_t *points = line_points[index];
        points[0] = (lv_point_precise_t){ x1 - left, y1 - top };
        points[1] = (lv_point_precise_t){ x2 - left, y2 - top };
        lv_obj_t *line = lv_line_create(artwork);
        lv_obj_remove_style_all(line);
        decorative(line);
        lv_obj_set_size(line, abs(x2 - x1) + 1, abs(y2 - y1) + 1);
        lv_obj_set_pos(line, left, top);
        lv_line_set_points(line, points, 2);
        lv_obj_set_style_line_color(line, color, 0);
        lv_obj_set_style_line_width(line, stroke, 0);
        lv_obj_set_style_line_rounded(line, true, 0);
        return line;
    }
    if (x < 0 || y < 0) {
        return NULL;
    }
    if (strcmp(type, "text") == 0) {
        lv_obj_t *text = label(artwork, &lv_font_montserrat_16, 0xf8fafc);
        decorative(text);
        const char *value = field_text(shape, "s");
        lv_label_set_text_fmt(text, "%.*s", utf8_prefix(value, 144), value);
        lv_obj_set_style_text_color(text, color, 0);
        lv_obj_align(text, LV_ALIGN_CENTER, x - SCREEN_SIZE / 2, y - SCREEN_SIZE / 2);
        return text;
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
        return arc;
    } else if (strcmp(type, "circle") == 0 || strcmp(type, "rect") == 0 || strcmp(type, "btn") == 0) {
        const bool circle = type[0] == 'c';
        const bool button = type[0] == 'b';
        int width = circle ? 2 * radius : field_num(shape, "w", 1, SCREEN_SIZE, 0);
        int height = circle ? 2 * radius : field_num(shape, "h", 1, SCREEN_SIZE, 0);
        if (!width || !height) {
            return NULL;
        }
        lv_obj_t *obj = plain_box(artwork, circle ? x - radius : x, circle ? y - radius : y, width, height);
        lv_obj_set_style_radius(obj, circle ? LV_RADIUS_CIRCLE : button ? field_num(shape, "r", 0, 180, 14) : radius, 0);
        if (button || cJSON_IsTrue(cJSON_GetObjectItemCaseSensitive(shape, "fill"))) {
            lv_obj_set_style_bg_color(obj, color, 0);
            lv_obj_set_style_bg_opa(obj, LV_OPA_COVER, 0);
        } else {
            lv_obj_set_style_border_color(obj, color, 0);
            lv_obj_set_style_border_width(obj, circle ? stroke : 2, 0);
        }
        if (button) {
            lv_obj_t *text = label(obj, &lv_font_montserrat_16, lv_color_brightness(color) > 140 ? 0x251c3b : 0xfffaff);
            lv_obj_set_size(text, width > 8 ? width - 8 : width, LV_MIN(height, 20));
            lv_obj_set_style_text_align(text, LV_TEXT_ALIGN_CENTER, 0);
            lv_label_set_long_mode(text, LV_LABEL_LONG_MODE_DOTS);
            const char *value = field_text(shape, "s");
            lv_label_set_text_fmt(text, "%.*s", utf8_prefix(value, 144), value);
            lv_obj_center(text);
        }
        return obj;
    }
    return NULL;
}

static void on_shape_tap(lv_event_t *event)
{
    shape_action_t *action = lv_event_get_user_data(event);
    if (action->reply_to) {
        if (!start_intercom(action->reply_to)) {
            set_diagram_subtitle(!online ? "Waiting for Wi-Fi" : !audio_ready ? "Sound unavailable" :
                                "Busy. Try again!");
        }
        return;
    }
    clear_highlight(highlight_timer);
    highlighted = lv_event_get_target(event);
    lv_obj_add_state(highlighted, LV_STATE_USER_1);
    lv_timer_reset(highlight_timer);
    lv_timer_resume(highlight_timer);
    if (action->ask && online) {
        if (conversation_ask(action->ask) == ESP_OK) {
            diagram_asking = true;
            if (action->tap) {
                lv_label_set_text_fmt(diagram_subtitle, "%s\nAsking...", action->tap);
                lv_obj_set_hidden(diagram_info, false);
            } else {
                set_diagram_subtitle("Asking...");
            }
            return;
        }
        set_diagram_subtitle(action->tap ? action->tap : "Busy. Try again!");
    } else {
        set_diagram_subtitle(action->tap ? action->tap : "Waiting for Wi-Fi");
    }
    conversation_chime();
}

static void add_shape_action(lv_obj_t *obj, const cJSON *shape, unsigned index)
{
    const char *tap = field_text(shape, "tap");
    const char *ask = field_text(shape, "ask");
    shape_action_t *action = &shape_actions[index];
    action->tap = *tap && strnlen(tap, 257) <= 256 ? strdup(tap) : NULL;
    action->ask = *ask && strnlen(ask, CONVERSATION_QUESTION_MAX + 1) <= CONVERSATION_QUESTION_MAX
                  ? strdup(ask) : NULL;
    const char *reply_to = field_text(shape, "reply_to");
    if (strcmp(field_text(shape, "t"), "btn") == 0 && bridge_client_valid_target(reply_to)) {
        action->reply_to = strdup(reply_to);
    }
    if (!action->tap && !action->ask && !action->reply_to) {
        return;
    }
    lv_obj_set_clickable(obj, true);
    lv_obj_set_ext_click_area(obj, 10);
    lv_obj_set_style_outline_color(obj, lv_color_hex(0xffa6dc), 0);
    lv_obj_set_style_outline_width(obj, 2, 0);
    lv_obj_set_style_outline_opa(obj, LV_OPA_60, 0);
    lv_obj_set_style_outline_color(obj, lv_color_hex(0xffed8a), LV_STATE_PRESSED);
    lv_obj_set_style_outline_color(obj, lv_color_hex(0xffed8a), LV_STATE_USER_1);
    lv_obj_set_style_outline_width(obj, 4, LV_STATE_USER_1);
    lv_obj_set_style_outline_opa(obj, LV_OPA_COVER, LV_STATE_USER_1);
    lv_obj_add_event_cb(obj, on_shape_tap, LV_EVENT_CLICKED, action);
}

static esp_err_t show_screen(const cJSON *screen)
{
    const cJSON *shapes = cJSON_GetObjectItemCaseSensitive(screen, "shapes");
    if (!cJSON_IsObject(screen) || (shapes && (!cJSON_IsArray(shapes) ||
                                              cJSON_GetArraySize(shapes) > MAX_SHAPES))) {
        return ESP_ERR_INVALID_ARG;
    }
    wake_locked();
    hide_screen();
    lv_obj_set_style_bg_color(diagram, field_color(screen, "bg", 0x0b1020), 0);
    const char *title = field_text(screen, "title");
    lv_label_set_text_fmt(diagram_title, "%.*s", utf8_prefix(title, 100), title);
    set_diagram_subtitle(field_text(screen, "subtitle"));
    // Line widgets borrow their points; the old widgets were deleted before reusing these slots.
    unsigned index = 0;
    const cJSON *shape;
    cJSON_ArrayForEach(shape, shapes) {
        lv_obj_t *obj = draw_shape(shape, index);
        if (obj) {
            add_shape_action(obj, shape, index);
        }
        ++index;
    }
    lv_obj_set_hidden(diagram, false);
    update_pages();
    return ESP_OK;
}

esp_err_t ui_init(const char *device_suffix, const uint8_t mac[6], ui_action_cb_t action_cb)
{
    lv_display_t *display = bsp_display_start();
    if (!display || !bsp_display_get_input_dev() || !bsp_display_lock(0)) {
        return ESP_FAIL;
    }
    dispatch = action_cb;
    lv_obj_t *screen = lv_display_get_screen_active(display);
    lv_obj_set_style_bg_color(screen, lv_color_hex(0x16132b), 0);
    lv_obj_set_scrollable(screen, false);

    cat = plain_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    lv_obj_set_clickable(cat, true);
    lv_obj_add_event_cb(cat, on_click, LV_EVENT_CLICKED, NULL);
    cat_face_init(cat, mac);
    lv_obj_t *caption_pill = plain_box(cat, 78, 244, 204, 34);
    lv_obj_set_style_radius(caption_pill, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_bg_color(caption_pill, lv_color_hex(0xfffaee), 0);
    lv_obj_set_style_bg_opa(caption_pill, LV_OPA_COVER, 0);
    lv_obj_set_style_border_width(caption_pill, 3, 0);
    lv_obj_set_style_border_color(caption_pill, lv_color_hex(CAT_INK), 0);
    caption = label(caption_pill, &lv_font_montserrat_16, CAT_INK);
    lv_obj_set_size(caption, 190, 20);
    lv_obj_set_style_text_align(caption, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(caption, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_center(caption);
    cat_text = named_pill(cat, "Text", -39, 289, 0xfffaee);
    reset = named_pill(cat, "New", 39, 289, cat_face_accent());

    chat = plain_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    lv_obj_t *title = label(chat, &lv_font_montserrat_24, 0xffe8fb);
    lv_label_set_text_fmt(title, "%s (%s)", cat_face_name(), device_suffix);
    lv_obj_align(title, LV_ALIGN_TOP_MID, 0, 31);
    network_label = label(chat, &lv_font_montserrat_16, 0xc2b4ee);
    lv_label_set_text(network_label, "Connecting...");
    lv_obj_align(network_label, LV_ALIGN_TOP_MID, 0, 61);

    scroll = lv_obj_create(chat);
    lv_obj_set_size(scroll, 232, 158);
    lv_obj_align(scroll, LV_ALIGN_TOP_MID, 0, 90);
    lv_obj_set_style_bg_color(scroll, lv_color_hex(0x292042), 0);
    lv_obj_set_style_border_width(scroll, 0, 0);
    lv_obj_set_style_radius(scroll, 18, 0);
    lv_obj_set_style_pad_all(scroll, 13, 0);
    lv_obj_set_scroll_dir(scroll, LV_DIR_VER);
    lv_obj_set_scrollbar_mode(scroll, LV_SCROLLBAR_MODE_AUTO);
    message_label = label(scroll, &lv_font_montserrat_16, 0xe2edf6);
    lv_obj_set_width(message_label, lv_pct(100));
    lv_label_set_long_mode(message_label, LV_LABEL_LONG_WRAP);

    lv_label_set_text(message_label, "Our chat will appear here.");
    talk = pill(chat, 166, 36, 252, 0x5444a3);
    talk_label = label(talk, &lv_font_montserrat_20, 0xf1f6fb);
    lv_obj_center(talk_label);
    chat_cat = named_pill(chat, "Cat", -39, CHAT_ACTION_Y, 0xfffaee);
    chat_new = named_pill(chat, "New", 39, CHAT_ACTION_Y, cat_face_accent());
    chat_peer = named_pill(chat, "", 0, CHAT_ACTION_Y, cat_face_accent());
    lv_obj_set_width(chat_peer, 90);
    chat_peer_label = lv_obj_get_child(chat_peer, 0);
    lv_obj_set_width(chat_peer_label, 82);
    lv_obj_set_style_text_align(chat_peer_label, LV_TEXT_ALIGN_CENTER, 0);
    lv_label_set_long_mode(chat_peer_label, LV_LABEL_LONG_MODE_DOTS);
    lv_obj_set_hidden(chat_peer, true);
    init_diagram(screen);
    init_volume(screen);
    lv_indev_add_event_cb(bsp_display_get_input_dev(), touch_started, LV_EVENT_PRESSED, NULL);
    last_activity = lv_tick_get();
    lv_timer_create(idle_check, 500, NULL);
    notice_timer = lv_timer_create(restore_caption, 2200, NULL);
    set_phase(UI_IDLE, "Starting…");
    bsp_display_unlock();

    esp_err_t err = bsp_display_brightness_set(85);
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
    if (phase == UI_IDLE) {
        set_phase(UI_IDLE, NULL);
    } else {
        update_controls();
    }
    bsp_display_unlock();
}

void ui_set_audio_ready(bool ready)
{
    bsp_display_lock(0);
    audio_ready = ready;
    set_phase(UI_IDLE, NULL);
    bsp_display_unlock();
}

void ui_set_peer(const char *name)
{
    bsp_display_lock(0);
    snprintf(displayed_peer, sizeof(displayed_peer), "%s", name);
    bool known = displayed_peer[0] != '\0';
    lv_label_set_text_fmt(chat_peer_label, "Msg %s", displayed_peer);
    lv_obj_set_hidden(chat_peer, !known);
    lv_obj_align(chat_cat, LV_ALIGN_TOP_MID, known ? -81 : -39, CHAT_ACTION_Y);
    lv_obj_align(chat_new, LV_ALIGN_TOP_MID, known ? 81 : 39, CHAT_ACTION_Y);
    bsp_display_unlock();
}

void ui_show_intercom_sent(const char *target)
{
    char message[sizeof("Sent to !") + BRIDGE_TARGET_CAP];
    snprintf(message, sizeof(message), "Sent to %s!", target);
    bsp_display_lock(0);
    set_phase(UI_IDLE, message);
    lv_timer_reset(notice_timer);
    lv_timer_resume(notice_timer);
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
    lv_label_set_text_fmt(message_label, "You: %.*s\n\nAI: %.*s",
                          utf8_prefix(transcript, 1024), transcript, utf8_prefix(reply, 4096), reply);
    lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    if (show_screen(screen) != ESP_OK) {
        hide_screen();
    }
    bsp_display_unlock();
}

void ui_clear_reply(void)
{
    bsp_display_lock(0);
    lv_label_set_text(message_label, "Our chat will appear here.");
    lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    hide_screen();
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
    text_view = false;
    hide_screen();
    bsp_display_unlock();
}
