#include "cat_face.h"

typedef enum { MINT, PLUM, SUNNY, CAKE } character_t;

typedef struct {
    const char *name;
    uint32_t face;
    uint32_t marking;
    uint32_t pink;
    uint32_t accent;
} palette_t;

static const palette_t palettes[] = {
    [MINT] = { "Mint", 0x7be3c6, 0x2cb5a0, 0xff7a90, 0xffd166 },
    [PLUM] = { "Plum", 0xd8c7ff, 0x9f7aea, 0xff9eb5, 0xdcb5ff },
    [SUNNY] = { "Sunny", 0xfcd34d, 0xea580c, 0xfb7185, 0xffbb6e },
    [CAKE] = { "Cake", 0xfff3d1, 0xf59e0b, 0xff85a1, 0xfacc15 },
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
static unsigned bounce;
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

static void bean(brush_t b, int x, int y, int w, int h, uint32_t color, unsigned border)
{
    block(b, x, y, w, h, LV_RADIUS_CIRCLE, color, border);
}

static void ring(brush_t b, int inset, unsigned width, uint32_t color)
{
    lv_draw_rect_dsc_t shape;
    lv_draw_rect_dsc_init(&shape);
    shape.radius = LV_RADIUS_CIRCLE;
    shape.bg_opa = LV_OPA_TRANSP;
    shape.border_color = lv_color_hex(color);
    shape.border_width = width;
    shape.border_opa = LV_OPA_COVER;
    const lv_area_t area = { b.x + inset, b.y + inset, b.x + 359 - inset, b.y + 359 - inset };
    lv_draw_rect(b.layer, &shape, &area);
}

static void triangle(brush_t b, lv_point_t a, lv_point_t c, lv_point_t d, uint32_t color)
{
    lv_draw_triangle_dsc_t tri;
    lv_draw_triangle_dsc_init(&tri);
    tri.p[0] = (lv_point_precise_t){ b.x + a.x, b.y + a.y };
    tri.p[1] = (lv_point_precise_t){ b.x + c.x, b.y + c.y };
    tri.p[2] = (lv_point_precise_t){ b.x + d.x, b.y + d.y };
    tri.color = lv_color_hex(color);
    lv_draw_triangle(b.layer, &tri);
}

static void diamond(brush_t b, int x, int y, int w, int h, uint32_t color)
{
    const lv_point_t left = { x - w, y }, right = { x + w, y };
    triangle(b, left, (lv_point_t){ x, y - h }, right, color);
    triangle(b, left, right, (lv_point_t){ x, y + h }, color);
}

static void sparkle(brush_t b, int x, int y, int radius, uint32_t color)
{
    diamond(b, x, y, 4, radius + 2, CAT_INK);
    diamond(b, x, y, radius + 2, 4, CAT_INK);
    diamond(b, x, y, 3, radius, color);
    diamond(b, x, y, radius, 3, color);
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

static void curve(brush_t b, int x, int y, int radius, int start, int end, uint32_t color, unsigned width)
{
    lv_draw_arc_dsc_t arc;
    lv_draw_arc_dsc_init(&arc);
    arc.center = (lv_point_t){ b.x + x, b.y + y };
    arc.radius = radius;
    arc.start_angle = start;
    arc.end_angle = end;
    arc.color = lv_color_hex(color);
    arc.width = width;
    arc.rounded = true;
    lv_draw_arc(b.layer, &arc);
}

static void markings(brush_t b, const palette_t *p)
{
    switch (character) {
    case CAKE:
        bean(b, 141, 28, 17, 30, p->marking, 3);
        bean(b, 170, 25, 20, 41, p->marking, 3);
        bean(b, 202, 28, 17, 30, p->marking, 3);
        block(b, 210, 103, 71, 68, 24, p->marking, 3);
        break;
    case MINT:
        diamond(b, 180, 52, 20, 21, CAT_INK);
        diamond(b, 180, 52, 15, 16, p->marking);
        block(b, 176, 48, 8, 8, 0, p->accent, 0);
        block(b, 153, 216, 21, 15, 0, p->pink, 3);
        block(b, 186, 216, 21, 15, 0, p->pink, 3);
        block(b, 172, 219, 16, 10, 0, p->pink, 3);
        break;
    case PLUM:
        bean(b, 156, 33, 42, 42, CAT_INK, 0);
        bean(b, 159, 36, 36, 36, p->marking, 0);
        bean(b, 172, 29, 34, 34, p->face, 0);
        sparkle(b, 123, 182, 6, p->accent);
        sparkle(b, 237, 182, 6, p->accent);
        break;
    case SUNNY:
        diamond(b, 180, 52, 25, 24, CAT_INK);
        diamond(b, 180, 52, 20, 19, p->marking);
        bean(b, 168, 40, 24, 24, p->accent, 3);
        for (int i = 0; i < 3; ++i) {
            bean(b, 108 + i * 9, 202 - i * 3, 4, 4, CAT_INK, 0);
            bean(b, 248 - i * 9, 202 - i * 3, 4, 4, CAT_INK, 0);
        }
        break;
    }
}

static void eyes(brush_t b)
{
    if (sleeping) {
        curve(b, 116, 137, 19, 0, 180, CAT_INK, 5);
        curve(b, 244, 137, 19, 0, 180, CAT_INK, 5);
        return;
    }
    if (mood == UI_BUSY) {
        bean(b, 93, 112, 46, 46, 0xfffdf4, 4);
        bean(b, 112 + (bounce ? 4 : 0), 124, 10, 10, CAT_INK, 0);
        stroke(b, 230, 138, 245, 132, 6);
        stroke(b, 245, 132, 257, 136, 6);
        return;
    }
    for (int i = 0; i < 2; ++i) {
        int x = i ? 244 : 116;
        if (mood == UI_SPEAKING) {
            curve(b, x, 143, 19, 205, 335, CAT_INK, 5);
        } else if (blink) {
            bean(b, x - 14, 136, 28, 6, CAT_INK, 0);
        } else {
            int width = mood == UI_RECORDING ? 32 : 26;
            int height = mood == UI_RECORDING ? 35 : 30;
            bean(b, x + gaze - width / 2, 138 - height / 2, width, height, CAT_INK, 0);
            bean(b, x + gaze - 6, 129, 5, 5, 0xfffdf4, 0);
        }
    }
}

static void sleep_z(brush_t b, int x, int y, int size)
{
    stroke(b, x, y, x + size, y, 3);
    stroke(b, x + size, y, x, y + size, 3);
    stroke(b, x, y + size, x + size, y + size, 3);
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

static void mouth(brush_t b, uint32_t pink)
{
    if (mood == UI_RECORDING) {
        bean(b, 167, 176, 26, 32, 0x7f1d3a, 4);
        bean(b, 175, 194, 10, 7, pink, 0);
    } else if (mood == UI_BUSY) {
        stroke(b, 163, 182, 172, 178, 5);
        stroke(b, 172, 178, 184, 185, 5);
        stroke(b, 184, 185, 196, 179, 5);
    } else if (mood == UI_SPEAKING && talking) {
        // A flat top over the rounded lower bowl gives the open smile its D shape.
        bean(b, 155, 174, 50, 33, CAT_INK, 0);
        block(b, 155, 174, 50, 15, 4, CAT_INK, 0);
        bean(b, 159, 178, 42, 25, 0x7a1c3c, 0);
        block(b, 159, 178, 42, 10, 1, 0x7a1c3c, 0);
        bean(b, 170, 194, 20, 8, pink, 0);
    } else {
        curve(b, 167, 174, 13, 0, 165, CAT_INK, 5);
        curve(b, 193, 174, 13, 15, 180, CAT_INK, 5);
    }
}

static void draw_face(lv_event_t *event)
{
    lv_area_t area;
    lv_obj_get_coords(face, &area);
    const brush_t b = { lv_event_get_layer(event), area.x1, area.y1 };
    const palette_t *p = &palettes[character];
    ring(b, 6, 5, CAT_INK);
    if (mood != UI_IDLE) {
        uint32_t accent = mood == UI_RECORDING ? 0xff5964 : mood == UI_BUSY ? 0xfacc15 : 0x38d6f4;
        ring(b, 13, 7, accent);
    }
    triangle(b, (lv_point_t){72, 56}, (lv_point_t){85, 108}, (lv_point_t){130, 77}, CAT_INK);
    triangle(b, (lv_point_t){79, 64}, (lv_point_t){88, 100}, (lv_point_t){120, 78}, p->pink);
    triangle(b, (lv_point_t){288, 56}, (lv_point_t){275, 108}, (lv_point_t){230, 77}, CAT_INK);
    triangle(b, (lv_point_t){281, 64}, (lv_point_t){272, 100}, (lv_point_t){240, 78}, p->pink);
    bean(b, 84, 169, 31, 31, p->pink, 0);
    bean(b, 245, 169, 31, 31, p->pink, 0);
    stroke(b, 49, 176, 84, 184, 4);
    stroke(b, 50, 202, 85, 193, 4);
    stroke(b, 276, 184, 311, 176, 4);
    stroke(b, 275, 193, 310, 202, 4);
    markings(b, p);
    eyes(b);
    bean(b, 172, 156, 16, 11, p->pink, 3);
    mouth(b, p->pink);
    battery(b);
    if (sleeping) {
        sleep_z(b, 207, 91 - bounce, 8);
        sleep_z(b, 223, 77 + bounce, 13);
    } else if (mood == UI_BUSY) {
        bean(b, 218, 98 - bounce, 10, 10, 0xfffdf4, 3);
        bean(b, 227, 82 + bounce, 14, 14, 0xfffdf4, 3);
    } else if (mood == UI_RECORDING) {
        sparkle(b, 74, 132, 10, 0xffd166);
        sparkle(b, 285, 127, 11, 0xffd166);
    }
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
    if (lv_obj_is_hidden(face)) {
        return;
    }
    uint32_t now = lv_tick_get();
    uint32_t age = now - face_start;
    uint32_t elapsed = now - phase_start;
    bool next_blink = age % 3500 >= 3350;
    int next_gaze = age % 4800 < 4200 ? 0 : age / 4800 % 2 ? -4 : 4;
    if (sleeping) {
        unsigned step = elapsed / 200 % 8;
        unsigned lift = step <= 4 ? step : 8 - step;
        if (bounce != lift) {
            bounce = lift;
            redraw(204, 73, 239, 102);
        }
    } else if (mood == UI_IDLE || mood == UI_RECORDING) {
        if (blink != next_blink || gaze != next_gaze) {
            blink = next_blink;
            gaze = next_gaze;
            redraw(94, 118, 267, 157);
        }
    } else if (mood == UI_SPEAKING) {
        bool open = elapsed % 300 < 150;
        if (talking != open) {
            talking = open;
            redraw(153, 171, 207, 210);
        }
    } else {
        unsigned step = elapsed / 100 % 6;
        unsigned lift = step < 3 ? step * 2 : (6 - step) * 2;
        if (bounce != lift) {
            bounce = lift;
            redraw(216, 80, 247, 109);
            redraw(109, 122, 130, 137);
        }
    }
}

void cat_face_init(lv_obj_t *parent, const uint8_t mac[6])
{
    face = parent;
    character = (mac[4] ^ mac[5]) % 4;
    lv_obj_set_style_bg_color(face, lv_color_hex(palettes[character].face), 0);
    lv_obj_set_style_bg_opa(face, LV_OPA_COVER, 0);
    lv_obj_add_event_cb(face, draw_face, LV_EVENT_DRAW_MAIN, NULL);
    face_start = phase_start = lv_tick_get();
    lv_timer_create(animate, 50, NULL);
}

void cat_face_set_phase(ui_phase_t phase)
{
    if (mood != phase) {
        mood = phase;
        phase_start = lv_tick_get();
        talking = phase == UI_SPEAKING;
        bounce = 0;
        lv_obj_invalidate(face);
    }
    animate(NULL);
}

void cat_face_set_sleeping(bool asleep)
{
    sleeping = asleep;
    phase_start = lv_tick_get();
    bounce = 0;
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
