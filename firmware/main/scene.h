#pragma once

#include "cJSON.h"
#include "gfx.h"

enum { SCENE_MAX_NODES = 48, SCENE_ID_CAP = 64, SCENE_TEXT_CAP = 257 };
typedef enum { NODE_CIRCLE, NODE_RECT, NODE_LINE, NODE_ARC, NODE_TEXT, NODE_BTN, NODE_TRI } node_kind_t;
typedef enum { NODE_PASSIVE, NODE_TAP, NODE_DRAG } node_input_t;

typedef struct {
    char id[SCENE_ID_CAP], s[SCENE_TEXT_CAP];
    node_kind_t kind;
    node_input_t on;
    int16_t x, y, x1, y1, x2, y2, x3, y3, w, h, r, sw, a1, a2, fs;
    gfx_color_t c, bc, tc;
    bool fill, vis, border, text_color;
} scene_node_t;

typedef struct {
    scene_node_t nodes[SCENE_MAX_NODES];
    unsigned count;
    gfx_color_t bg;
    uint32_t rev;
} scene_frame_t;

typedef struct {
    scene_frame_t frames[2];
    unsigned active;
    bool ready;
} scene_t;

bool scene_apply_json(scene_t *scene, const cJSON *msg);
void scene_render(const scene_t *scene, uint16_t *fb);
void scene_render_offline(uint16_t *fb, const char *character, const char *ip, const char *status);
const scene_node_t *scene_hit_test(const scene_t *scene, int x, int y);
