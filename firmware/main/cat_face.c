#include "cat_face.h"

typedef enum { MINT, PLUM, SUNNY, CAKE } character_t;

typedef struct {
    const char *name;
    uint32_t face;
    uint32_t marking;
    uint32_t accent;
} palette_t;

static const palette_t palettes[] = {
    [MINT] = { "Mint", 0x7be3c6, 0x228e81, 0xffd166 },
    [PLUM] = { "Plum", 0xd8c7ff, 0x9f7aea, 0xdcb5ff },
    [SUNNY] = { "Sunny", 0xfcd34d, 0xea580c, 0xffbb6e },
    [CAKE] = { "Cake", 0xfff3d1, 0xf59e0b, 0xfacc15 },
};

enum {
    LEFT_EYE_X = 112, RIGHT_EYE_X = 248, EYE_Y = 142,
    MAX_EYE_W = 40, MAX_EYE_H = 44, GAZE_SHIFT = 4,
    MOUTH_X = 140, MOUTH_Y = 189, MOUTH_W = 80, MOUTH_H = 40,
};

typedef struct {
    lv_layer_t *layer;
    int x;
    int y;
} brush_t;

static lv_obj_t *face;
static character_t character;
static ui_phase_t mood;
static uint32_t phase_start;
static uint32_t face_start;
static int gaze;
static bool blink;
static bool talking;
static bool sleeping;
static int battery_pct = -1;
static bool charging;

const char *cat_face_name(void)
{
    return palettes[character].name;
}

uint32_t cat_face_accent(void)
{
    return palettes[character].accent;
}

static void block(brush_t b, int x, int y, int w, int h, int radius, uint32_t color, unsigned border)
{
    lv_draw_rect_dsc_t shape;
    lv_draw_rect_dsc_init(&shape);
    shape.radius = radius;
    shape.bg_color = lv_color_hex(color);
    shape.bg_opa = LV_OPA_COVER;
    shape.border_color = lv_color_hex(CAT_INK);
    shape.border_width = border;
    shape.border_opa = LV_OPA_COVER;
    const lv_area_t area = { b.x + x, b.y + y, b.x + x + w - 1, b.y + y + h - 1 };
    lv_draw_rect(b.layer, &shape, &area);
}

static void bean(brush_t b, int x, int y, int w, int h, uint32_t color)
{
    block(b, x, y, w, h, LV_RADIUS_CIRCLE, color, 0);
}

static void ring(brush_t b)
{
    const int inset = 7;
    const uint32_t color = mood == UI_RECORDING ? 0xff5964 : mood == UI_BUSY ? 0xf59e0b :
                           mood == UI_SPEAKING ? 0x0284c7 : CAT_INK;
    lv_draw_rect_dsc_t shape;
    lv_draw_rect_dsc_init(&shape);
    shape.radius = LV_RADIUS_CIRCLE;
    shape.bg_opa = LV_OPA_TRANSP;
    shape.border_color = lv_color_hex(color);
    shape.border_width = mood == UI_IDLE ? 3 : 5;
    shape.border_opa = mood == UI_IDLE ? LV_OPA_50 : LV_OPA_80;
    const lv_area_t area = { b.x + inset, b.y + inset, b.x + 359 - inset, b.y + 359 - inset };
    lv_draw_rect(b.layer, &shape, &area);
}

static void stroke(brush_t b, int x1, int y1, int x2, int y2, unsigned width)
{
    lv_draw_line_dsc_t line;
    lv_draw_line_dsc_init(&line);
    line.p1 = (lv_point_precise_t){ b.x + x1, b.y + y1 };
    line.p2 = (lv_point_precise_t){ b.x + x2, b.y + y2 };
    line.color = lv_color_hex(CAT_INK);
    line.width = width;
    line.round_start = line.round_end = true;
    lv_draw_line(b.layer, &line);
}

static void make_ink_arc(brush_t b, int x, int y, int radius, int start, int end)
{
    lv_draw_arc_dsc_t arc;
    lv_draw_arc_dsc_init(&arc);
    arc.center = (lv_point_t){ b.x + x, b.y + y };
    arc.radius = radius;
    arc.start_angle = start;
    arc.end_angle = end;
    arc.color = lv_color_hex(CAT_INK);
    arc.width = 7;
    arc.rounded = true;
    lv_draw_arc(b.layer, &arc);
}

static void markings(brush_t b, const palette_t *p)
{
    switch (character) {
    case CAKE:
        bean(b, 147, 66, 14, 24, p->marking);
        bean(b, 173, 61, 14, 33, p->marking);
        bean(b, 199, 66, 14, 24, p->marking);
        break;
    case MINT:
        bean(b, 156, 72, 48, 12, p->marking);
        break;
    case PLUM:
        bean(b, 162, 61, 36, 36, p->marking);
        bean(b, 175, 55, 32, 32, p->face);
        break;
    case SUNNY:
        bean(b, 164, 62, 32, 32, p->marking);
        break;
    }
}

static void eyes(brush_t b)
{
    for (int i = 0; i < 2; ++i) {
        int x = i ? RIGHT_EYE_X : LEFT_EYE_X;
        if (sleeping) {
            make_ink_arc(b, x, EYE_Y - 4, 21, 0, 180);
        } else if (mood == UI_SPEAKING) {
            make_ink_arc(b, x, EYE_Y + 5, 22, 205, 335);
        } else if (mood == UI_BUSY ? i != 0 : blink) {
            bean(b, x - 17, EYE_Y - 4, 34, 8, CAT_INK);
        } else {
            int width = mood == UI_RECORDING ? MAX_EYE_W : 34;
            int height = mood == UI_RECORDING ? MAX_EYE_H : 38;
            int shift = mood == UI_BUSY ? 0 : gaze;
            bean(b, x + shift - width / 2, EYE_Y - height / 2, width, height, CAT_INK);
        }
    }
}

static void battery(brush_t b)
{
    if (battery_pct < 0) {
        return;
    }
    block(b, 258, 46, 6, 7, 2, CAT_INK, 0);
    block(b, 229, 41, 31, 17, 5, 0xfffaee, 2);
    uint32_t fill = charging || battery_pct >= 20 ? 0x39d1a0 : battery_pct < 10 ? 0xff5964 : 0xfacc15;
    int width = battery_pct * 25 / 100;
    if (width > 0) {
        block(b, 232, 44, width, 11, 2, fill, 0);
    }
    if (charging) {
        stroke(b, 247, 44, 242, 49, 2);
        stroke(b, 242, 49, 247, 49, 2);
        stroke(b, 247, 49, 243, 54, 2);
    }
}

static void mouth(brush_t b)
{
    if (mood == UI_RECORDING) {
        bean(b, 163, 191, 34, 34, CAT_INK);
    } else if (mood == UI_BUSY) {
        stroke(b, 166, 204, 194, 200, 6);
    } else if (mood == UI_SPEAKING && talking) {
        bean(b, MOUTH_X, MOUTH_Y, MOUTH_W, MOUTH_H, CAT_INK);
        block(b, MOUTH_X, MOUTH_Y, MOUTH_W, MOUTH_H / 2, 5, CAT_INK, 0);
        bean(b, 164, 214, 32, 9, 0xf47e92);
    } else {
        make_ink_arc(b, 163, 193, 21, 0, 165);
        make_ink_arc(b, 197, 193, 21, 15, 180);
    }
}

static void draw_face(lv_event_t *event)
{
    lv_area_t area;
    lv_obj_get_coords(face, &area);
    const brush_t b = { lv_event_get_layer(event), area.x1, area.y1 };
    ring(b);
    markings(b, &palettes[character]);
    eyes(b);
    stroke(b, 38, 177, 87, 188, 6);
    stroke(b, 41, 212, 87, 199, 6);
    stroke(b, 273, 188, 322, 177, 6);
    stroke(b, 273, 199, 319, 212, 6);
    bean(b, 165, 163, 30, 17, CAT_INK);
    mouth(b);
    battery(b);
}

static void redraw(int x1, int y1, int x2, int y2)
{
    lv_area_t area;
    lv_obj_get_coords(face, &area);
    const lv_area_t dirty = { area.x1 + x1, area.y1 + y1, area.x1 + x2, area.y1 + y2 };
    lv_obj_invalidate_area(face, &dirty);
}

static void animate(lv_timer_t *timer)
{
    if (sleeping || lv_obj_is_hidden(face)) {
        return;
    }
    uint32_t now = lv_tick_get();
    if (mood == UI_IDLE || mood == UI_RECORDING) {
        uint32_t age = now - face_start;
        bool next_blink = age % 3500 >= 3350;
        int next_gaze = age % 4800 < 4200 ? 0 : age / 4800 % 2 ? -GAZE_SHIFT : GAZE_SHIFT;
        if (blink != next_blink || gaze != next_gaze) {
            blink = next_blink;
            gaze = next_gaze;
            redraw(LEFT_EYE_X - MAX_EYE_W / 2 - GAZE_SHIFT, EYE_Y - MAX_EYE_H / 2,
                   RIGHT_EYE_X + MAX_EYE_W / 2 + GAZE_SHIFT - 1, EYE_Y + MAX_EYE_H / 2 - 1);
        }
    } else if (mood == UI_SPEAKING) {
        bool open = (now - phase_start) % 300 < 150;
        if (talking != open) {
            talking = open;
            redraw(MOUTH_X, MOUTH_Y, MOUTH_X + MOUTH_W - 1, MOUTH_Y + MOUTH_H - 1);
        }
    }
}

static void face_deleted(lv_event_t *event)
{
    lv_timer_delete(lv_event_get_user_data(event));
    face = NULL;
}

bool cat_face_init(lv_obj_t *parent, const uint8_t mac[6])
{
    if (!lv_obj_add_event_cb(parent, draw_face, LV_EVENT_DRAW_MAIN, NULL)) {
        return false;
    }
    lv_timer_t *timer = lv_timer_create(animate, 50, NULL);
    if (!timer) {
        return false;
    }
    if (!lv_obj_add_event_cb(parent, face_deleted, LV_EVENT_DELETE, timer)) {
        lv_timer_delete(timer);
        return false;
    }
    face = parent;
    character = (mac[4] ^ mac[5]) % 4;
    lv_obj_set_style_bg_color(face, lv_color_hex(palettes[character].face), 0);
    lv_obj_set_style_bg_opa(face, LV_OPA_COVER, 0);
    face_start = phase_start = lv_tick_get();
    return true;
}

void cat_face_set_phase(ui_phase_t phase)
{
    if (mood != phase) {
        mood = phase;
        phase_start = lv_tick_get();
        talking = phase == UI_SPEAKING;
        lv_obj_invalidate(face);
    }
    animate(NULL);
}

void cat_face_set_sleeping(bool asleep)
{
    sleeping = asleep;
    lv_obj_invalidate(face);
}

void cat_face_set_battery(int pct, bool is_charging)
{
    if (battery_pct != pct || charging != is_charging) {
        battery_pct = pct;
        charging = is_charging;
        redraw(227, 39, 265, 60);
    }
}
