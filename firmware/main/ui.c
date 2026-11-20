#include "ui.h"

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "bridge_client.h"
#include "bsp/esp_vocat.h"
#include "cat_face.h"
#include "conversation.h"
#include "ui_widgets.h"

enum { CHAT_ACTION_Y = 293 };

static lv_obj_t *cat, *cat_text, *caption, *reset;
static lv_obj_t *chat, *chat_cat, *chat_new, *chat_peer, *chat_peer_label, *network_label, *scroll, *message_label, *talk;
static lv_obj_t *diagram, *artwork, *diagram_title, *diagram_info, *diagram_subtitle, *diagram_talk, *diagram_back;
static lv_obj_t *highlighted, *volume_pill, *volume_label;
static lv_timer_t *volume_timer, *highlight_timer, *notice_timer;
typedef struct { char *tap; char *ask; char *reply_to; } shape_action_t;
static shape_action_t shape_actions[MAX_SHAPES];
static bridge_peer_t displayed_peer;
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
    if (!bsp_display_lock(0)) {
        return false;
    }
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

static bool can_intercom(void)
{
    return online && audio_ready && (phase == UI_IDLE || phase == UI_SPEAKING);
}

static void update_controls(void)
{
    bool recording = phase == UI_RECORDING, speaking = phase == UI_SPEAKING, ready = phase == UI_IDLE && online;
    const char *title = recording ? "Send" : speaking ? "Stop speech" : "Tap to Talk";
    lv_color_t color = lv_color_hex(recording ? 0x9b3045 : 0x5444a3);
    lv_obj_t *talk_buttons[] = { talk, diagram_talk };
    for (size_t i = 0; i < sizeof(talk_buttons) / sizeof(talk_buttons[0]); ++i) {
        lv_obj_t *button = talk_buttons[i];
        lv_label_set_text(lv_obj_get_child(button, 0), title);
        lv_obj_set_style_bg_color(button, color, LV_PART_MAIN);
        lv_obj_set_state(button, LV_STATE_DISABLED, !(recording || speaking || (ready && audio_ready)));
    }
    lv_obj_set_state(reset, LV_STATE_DISABLED, !ready);
    lv_obj_set_state(chat_new, LV_STATE_DISABLED, !ready);
    lv_obj_set_state(chat_peer, LV_STATE_DISABLED, !can_intercom());
}

static void set_diagram_subtitle(const char *message)
{
    lv_label_set_text_fmt(diagram_subtitle, "%.*s", ui_utf8_prefix(message, 256), message);
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
        if (phase == UI_IDLE && online && audio_ready) {
            snprintf(greeting, sizeof(greeting), "Tap %s!", cat_face_name());
            message = greeting;
        } else {
            message = phase == UI_RECORDING ? "Listening!" : phase == UI_BUSY ? "Hmm..." :
                      phase == UI_SPEAKING ? "Meow!" : audio_ready ? "Joining Wi-Fi..." : "Sound unavailable";
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

static void show_cat(void)
{
    text_view = false;
    hide_screen();
}

static bool start_intercom(const char *target, const char *character)
{
    if (!can_intercom() || conversation_start_intercom(target, character) != ESP_OK) {
        return false;
    }
    char message[sizeof("To !") + BRIDGE_TARGET_CAP];
    snprintf(message, sizeof(message), "To %s!", character);
    wake_locked();
    show_cat();
    set_phase(UI_RECORDING, message);
    return true;
}

static void talk_action(bool dismiss_diagram)
{
    ui_phase_t next;
    if (phase == UI_SPEAKING) {
        dispatch(UI_STOP_SPEAKING);
        return;
    }
    if (phase == UI_RECORDING && dispatch(UI_STOP_RECORDING)) {
        next = UI_BUSY;
    } else if (phase == UI_IDLE && online && audio_ready && dispatch(UI_START_RECORDING)) {
        next = UI_RECORDING;
    } else {
        return;
    }
    if (dismiss_diagram) {
        show_cat();
    }
    set_phase(next, NULL);
}

void ui_pet(void)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    if (!wake_locked()) {
        talk_action(false);
    }
    bsp_display_unlock();
}

static void on_click(lv_event_t *event)
{
    lv_obj_t *button = lv_event_get_target(event);
    if (button == diagram_back) {
        show_cat();
    } else if (button == cat_text || button == chat_cat) {
        text_view = button == cat_text;
        update_pages();
    } else if (button == chat_peer) {
        start_intercom(displayed_peer.id, displayed_peer.character);
    } else if (button == reset || button == chat_new) {
        if (dispatch(UI_NEW_CHAT)) {
            set_phase(UI_BUSY, "Freshening up...");
        }
    } else if (button != cat || !touch_woke) {
        talk_action(button == diagram_talk);
    }
}

static lv_obj_t *pill(lv_obj_t *parent, int width, int x, int y, uint32_t color,
                     const lv_font_t *font, const char *title)
{
    lv_obj_t *obj = parent ? lv_button_create(parent) : NULL;
    if (!obj) {
        return NULL;
    }
    lv_obj_t *text = ui_label(obj, font, title ? CAT_INK : 0xf1f6fb);
    if (!text || !lv_obj_add_event_cb(obj, on_click, LV_EVENT_CLICKED, NULL)) {
        lv_obj_delete(obj);
        return NULL;
    }
    lv_obj_set_size(obj, width, title ? 30 : 36);
    lv_obj_align(obj, LV_ALIGN_TOP_MID, x, y);
    lv_obj_set_style_radius(obj, LV_RADIUS_CIRCLE, 0);
    lv_obj_set_style_pad_all(obj, 0, 0);
    lv_obj_set_style_shadow_width(obj, 0, 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(color), 0);
    lv_obj_set_style_bg_color(obj, lv_color_hex(title ? 0xd7ccc9 : 0x252b34), LV_STATE_DISABLED);
    if (title) {
        ui_ink_border(obj);
        lv_obj_set_style_bg_color(obj, lv_color_hex(0xffcb9a), LV_STATE_PRESSED);
        lv_label_set_text(text, title);
    }
    lv_obj_center(text);
    return obj;
}

static lv_obj_t *named_pill(lv_obj_t *parent, const char *title, int x, int y, uint32_t color)
{
    return pill(parent, 68, x, y, color, &lv_font_montserrat_16, title);
}

static void hide_volume(lv_timer_t *timer)
{
    lv_obj_set_hidden(volume_pill, true);
    lv_timer_pause(timer);
}

void ui_show_volume(int level)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    wake_locked();
    lv_label_set_text_fmt(volume_label, "Volume %d%%", level);
    lv_obj_set_hidden(volume_pill, false);
    lv_timer_reset(volume_timer);
    lv_timer_resume(volume_timer);
    bsp_display_unlock();
}

void ui_set_battery(int pct, bool charging)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    cat_face_set_battery(pct, charging);
    bsp_display_unlock();
}

static bool init_diagram(lv_obj_t *screen)
{
    diagram = ui_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    artwork = ui_box(diagram, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    diagram_title = ui_label(diagram, &lv_font_montserrat_24, 0xf8fafc);
    diagram_info = ui_box(diagram, 65, 247, 230, 36);
    diagram_subtitle = ui_label(diagram_info, &lv_font_montserrat_16, 0xfff3fd);
    diagram_talk = pill(diagram, 142, 0, 286, 0x5444a3, &lv_font_montserrat_16, NULL);
    diagram_back = named_pill(diagram, "Cat", 0, 324, 0xfffaee);
    if (!artwork || !diagram_title || !diagram_subtitle || !diagram_talk || !diagram_back) {
        return false;
    }
    lv_obj_set_clickable(diagram, true);
    lv_obj_set_style_bg_opa(diagram, LV_OPA_COVER, 0);
    ui_fit_label(diagram_title, 180, 29);
    lv_obj_align(diagram_title, LV_ALIGN_TOP_MID, 0, 26);
    lv_obj_set_style_radius(diagram_info, 15, 0);
    lv_obj_set_style_bg_color(diagram_info, lv_color_hex(0x1b1630), 0);
    lv_obj_set_style_bg_opa(diagram_info, LV_OPA_90, 0);
    ui_fit_label(diagram_subtitle, 216, 34);
    lv_obj_center(diagram_subtitle);
    return true;
}

static void on_shape_tap(lv_event_t *event)
{
    shape_action_t *action = lv_event_get_user_data(event);
    if (action->reply_to) {
        bool sibling = displayed_peer.id[0] && (!strcasecmp(action->reply_to, displayed_peer.character) ||
                                                !strcasecmp(action->reply_to, displayed_peer.id));
        if (!start_intercom(sibling ? displayed_peer.id : action->reply_to,
                            sibling ? displayed_peer.character : action->reply_to)) {
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
    bool can_ask = action->ask && online;
    if (can_ask && conversation_ask(action->ask) == ESP_OK) {
        diagram_asking = true;
        if (action->tap) {
            lv_label_set_text_fmt(diagram_subtitle, "%s\nAsking...", action->tap);
            lv_obj_set_hidden(diagram_info, false);
        } else {
            set_diagram_subtitle("Asking...");
        }
        return;
    }
    set_diagram_subtitle(action->tap ? action->tap : can_ask ? "Busy. Try again!" : "Waiting for Wi-Fi");
    conversation_chime();
}

static void attach_shape_action(lv_obj_t *obj, const cJSON *item)
{
    const char *tap = ui_field_text(item, "tap"), *ask = ui_field_text(item, "ask");
    shape_action_t *action = &shape_actions[lv_obj_get_index(obj)];
    action->tap = *tap && strnlen(tap, 257) <= 256 ? strdup(tap) : NULL;
    action->ask = *ask && strnlen(ask, CONVERSATION_QUESTION_MAX + 1) <= CONVERSATION_QUESTION_MAX
                  ? strdup(ask) : NULL;
    const char *reply_to = ui_field_text(item, "reply_to");
    if (!strcmp(ui_field_text(item, "t"), "btn") && bridge_client_valid_target(reply_to)) {
        action->reply_to = strdup(reply_to);
    }
    if (!action->tap && !action->ask && !action->reply_to) {
        return;
    }
    if (!lv_obj_add_event_cb(obj, on_shape_tap, LV_EVENT_CLICKED, action)) {
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
    lv_obj_set_style_bg_color(diagram, lv_color_hex(ui_field_color(screen, "bg", 0x0b1020)), 0);
    const char *title = ui_field_text(screen, "title");
    lv_label_set_text_fmt(diagram_title, "%.*s", ui_utf8_prefix(title, 100), title);
    set_diagram_subtitle(ui_field_text(screen, "subtitle"));
    unsigned index = 0;
    const cJSON *shape;
    cJSON_ArrayForEach(shape, shapes) {
        lv_obj_t *obj = ui_draw_shape(artwork, shape, index++);
        if (obj) {
            attach_shape_action(obj, shape);
        }
    }
    lv_obj_set_hidden(diagram, false);
    update_pages();
    return ESP_OK;
}

static bool init_cat(lv_obj_t *screen, const uint8_t mac[6])
{
    cat = ui_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    if (!cat || !lv_obj_add_event_cb(cat, on_click, LV_EVENT_CLICKED, NULL) || !cat_face_init(cat, mac)) {
        return false;
    }
    lv_obj_set_clickable(cat, true);
    caption = ui_label(ui_comic_box(cat, 78, 244, 204, 34, 0xfffaee), &lv_font_montserrat_16, CAT_INK);
    cat_text = named_pill(cat, "Text", -39, 289, 0xfffaee);
    reset = named_pill(cat, "New", 39, 289, cat_face_accent());
    if (!caption || !cat_text || !reset) {
        return false;
    }
    ui_fit_label(caption, 190, 20);
    lv_obj_center(caption);
    return true;
}

static bool init_chat(lv_obj_t *screen, const char *device_suffix)
{
    chat = ui_box(screen, 0, 0, SCREEN_SIZE, SCREEN_SIZE);
    lv_obj_t *title = ui_label(chat, &lv_font_montserrat_24, 0xffe8fb);
    network_label = ui_label(chat, &lv_font_montserrat_16, 0xc2b4ee);
    scroll = chat ? lv_obj_create(chat) : NULL;
    message_label = ui_label(scroll, &lv_font_montserrat_16, 0xe2edf6);
    talk = pill(chat, 166, 0, 252, 0x5444a3, &lv_font_montserrat_20, NULL);
    chat_cat = named_pill(chat, "Cat", -39, CHAT_ACTION_Y, 0xfffaee);
    chat_new = named_pill(chat, "New", 39, CHAT_ACTION_Y, cat_face_accent());
    chat_peer = pill(chat, 90, 0, CHAT_ACTION_Y, cat_face_accent(), &lv_font_montserrat_16, "");
    if (!title || !network_label || !message_label || !talk || !chat_cat || !chat_new || !chat_peer) {
        return false;
    }
    lv_label_set_text_fmt(title, "%s (%s)", cat_face_name(), device_suffix);
    lv_obj_align(title, LV_ALIGN_TOP_MID, 0, 31);
    lv_label_set_text(network_label, "Connecting...");
    lv_obj_align(network_label, LV_ALIGN_TOP_MID, 0, 61);
    lv_obj_set_size(scroll, 232, 158);
    lv_obj_align(scroll, LV_ALIGN_TOP_MID, 0, 90);
    lv_obj_set_style_bg_color(scroll, lv_color_hex(0x292042), 0);
    lv_obj_set_style_border_width(scroll, 0, 0);
    lv_obj_set_style_radius(scroll, 18, 0);
    lv_obj_set_style_pad_all(scroll, 13, 0);
    lv_obj_set_scroll_dir(scroll, LV_DIR_VER);
    lv_obj_set_scrollbar_mode(scroll, LV_SCROLLBAR_MODE_AUTO);
    lv_obj_set_width(message_label, lv_pct(100));
    lv_label_set_long_mode(message_label, LV_LABEL_LONG_WRAP);

    lv_label_set_text(message_label, "Our chat will appear here.");
    chat_peer_label = lv_obj_get_child(chat_peer, 0);
    ui_fit_label(chat_peer_label, 82, LV_SIZE_CONTENT);
    lv_obj_set_hidden(chat_peer, true);
    return true;
}

esp_err_t ui_init(const char *device_suffix, const uint8_t mac[6], ui_action_cb_t action_cb)
{
    lv_display_t *display = bsp_display_start();
    lv_indev_t *input = bsp_display_get_input_dev();
    if (!display || !input || !bsp_display_lock(0)) {
        return ESP_FAIL;
    }
    dispatch = action_cb;
    lv_obj_t *screen = lv_display_get_screen_active(display);
    lv_timer_t *idle_timer = NULL;
    if (!screen || !init_cat(screen, mac) || !init_chat(screen, device_suffix) || !init_diagram(screen)) {
        goto fail;
    }
    lv_obj_set_style_bg_color(screen, lv_color_hex(0x16132b), 0);
    lv_obj_set_scrollable(screen, false);
    volume_pill = ui_comic_box(screen, 103, 214, 154, 34, cat_face_accent());
    volume_label = ui_label(volume_pill, &lv_font_montserrat_16, CAT_INK);
    if (!volume_label) {
        goto fail;
    }
    lv_obj_center(volume_label);
    volume_timer = lv_timer_create(hide_volume, 1500, NULL);
    highlight_timer = lv_timer_create(clear_highlight, 200, NULL);
    notice_timer = lv_timer_create(restore_caption, 2200, NULL);
    idle_timer = lv_timer_create(idle_check, 500, NULL);
    if (!volume_timer || !highlight_timer || !notice_timer || !idle_timer ||
        !lv_indev_add_event_cb(input, touch_started, LV_EVENT_PRESSED, NULL)) {
        goto fail;
    }
    hide_volume(volume_timer);
    hide_screen();
    last_activity = lv_tick_get();
    set_phase(UI_IDLE, "Starting…");
    bsp_display_unlock();
    return bsp_display_brightness_set(85);

fail:;
    lv_timer_t *timers[] = { idle_timer, notice_timer, highlight_timer, volume_timer };
    lv_obj_t *views[] = { volume_pill, diagram, chat, cat };
    for (size_t i = 0; i < sizeof(timers) / sizeof(timers[0]); ++i) {
        if (timers[i]) {
            lv_timer_delete(timers[i]);
        }
    }
    for (size_t i = 0; i < sizeof(views) / sizeof(views[0]); ++i) {
        if (views[i]) {
            lv_obj_delete(views[i]);
        }
    }
    bsp_display_unlock();
    return ESP_ERR_NO_MEM;
}

void ui_set_network(bool connected, const char *ip)
{
    if (!bsp_display_lock(0)) {
        return;
    }
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
    if (!bsp_display_lock(0)) {
        return;
    }
    audio_ready = ready;
    set_phase(UI_IDLE, NULL);
    bsp_display_unlock();
}

void ui_set_peer(const char *id, const char *character)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    snprintf(displayed_peer.id, sizeof(displayed_peer.id), "%s", id);
    snprintf(displayed_peer.character, sizeof(displayed_peer.character), "%s", character);
    bool known = displayed_peer.id[0] != '\0';
    lv_label_set_text_fmt(chat_peer_label, "Msg %s", displayed_peer.character);
    lv_obj_set_hidden(chat_peer, !known);
    lv_obj_align(chat_cat, LV_ALIGN_TOP_MID, known ? -81 : -39, CHAT_ACTION_Y);
    lv_obj_align(chat_new, LV_ALIGN_TOP_MID, known ? 81 : 39, CHAT_ACTION_Y);
    bsp_display_unlock();
}

void ui_finish_turn(bool (*mark_idle)(void), const char *message, const char *sent_to)
{
    if (!bsp_display_lock(0)) {
        mark_idle();
        return;
    }
    // Input callbacks use the same lock, so they cannot start a new turn before this caption is applied.
    if (mark_idle()) {
        char sent[sizeof("Sent to !") + BRIDGE_TARGET_CAP];
        if (sent_to) {
            snprintf(sent, sizeof(sent), "Sent to %s!", sent_to);
            message = sent;
        }
        set_phase(UI_IDLE, message);
        if (sent_to) {
            lv_timer_reset(notice_timer);
            lv_timer_resume(notice_timer);
        }
    }
    bsp_display_unlock();
}

void ui_set_phase(ui_phase_t next, const char *message)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    set_phase(next, message);
    bsp_display_unlock();
}

void ui_show_reply(const char *transcript, const char *reply, const cJSON *screen)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    lv_label_set_text_fmt(message_label, "You: %.*s\n\nAI: %.*s",
                          ui_utf8_prefix(transcript, 1024), transcript, ui_utf8_prefix(reply, 4096), reply);
    lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    if (show_screen(screen) != ESP_OK) {
        hide_screen();
    }
    bsp_display_unlock();
}

void ui_clear_reply(void)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    lv_label_set_text(message_label, "Our chat will appear here.");
    lv_obj_scroll_to_y(scroll, 0, LV_ANIM_OFF);
    hide_screen();
    bsp_display_unlock();
}

esp_err_t ui_show_screen(const cJSON *screen_json)
{
    if (!bsp_display_lock(0)) {
        return ESP_ERR_TIMEOUT;
    }
    esp_err_t err = show_screen(screen_json);
    bsp_display_unlock();
    return err;
}

void ui_hide_screen(void)
{
    if (!bsp_display_lock(0)) {
        return;
    }
    show_cat();
    bsp_display_unlock();
}
