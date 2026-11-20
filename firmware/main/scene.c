#include "scene.h"

#include <stdio.h>
#include <string.h>

static int min(int a, int b) { return a < b ? a : b; }
static int max(int a, int b) { return a > b ? a : b; }

static const char *str(const cJSON *obj, const char *key)
{
    return cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(obj, key));
}

static bool valid_id(const char *id)
{
    return id && *id && strlen(id) < SCENE_ID_CAP;
}

static int find(const scene_frame_t *frame, const char *id)
{
    for (unsigned i = 0; i < frame->count; ++i) {
        if (!strcmp(frame->nodes[i].id, id)) return (int)i;
    }
    return -1;
}

static bool read_kind(const char *name, node_kind_t *out)
{
    static const char *const types[] = { "circle", "rect", "line", "arc", "text", "btn", "tri" };
    for (unsigned i = 0; i < sizeof(types) / sizeof(types[0]); ++i) {
        if (!strcmp(name, types[i])) {
            *out = (node_kind_t)i;
            return true;
        }
    }
    return false;
}

static int number(const cJSON *obj, const char *key, int fallback, int lo, int hi)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(obj, key);
    return cJSON_IsNumber(value) && value->valuedouble >= lo && value->valuedouble <= hi
           ? value->valueint : fallback;
}

static int tri_coord(const cJSON *obj, const char *key, int fallback, int extent)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(obj, key);
    int coord = cJSON_IsNumber(value) ? value->valueint : fallback;
    return min(extent - 1, max(0, coord));
}

static bool update(scene_node_t *node, const cJSON *obj)
{
    if (!cJSON_IsObject(obj)) return false;
    const char *type = str(obj, "t"), *text = str(obj, "s"), *on = str(obj, "on");
    if (type && !read_kind(type, &node->kind)) return false;
    if (text) snprintf(node->s, sizeof(node->s), "%s", text);
    if (on) node->on = !strcmp(on, "tap") ? NODE_TAP : !strcmp(on, "drag") ? NODE_DRAG : NODE_PASSIVE;
    const cJSON *vis = cJSON_GetObjectItemCaseSensitive(obj, "vis"), *fill = cJSON_GetObjectItemCaseSensitive(obj, "fill");
    if (cJSON_IsBool(vis)) node->vis = cJSON_IsTrue(vis);
    if (cJSON_IsBool(fill)) node->fill = cJSON_IsTrue(fill);
    gfx_parse_color(str(obj, "c"), &node->c);
    const cJSON *bc = cJSON_GetObjectItemCaseSensitive(obj, "bc"), *tc = cJSON_GetObjectItemCaseSensitive(obj, "tc");
    if (bc) node->border = gfx_parse_color(cJSON_GetStringValue(bc), &node->bc);
    if (tc) node->text_color = gfx_parse_color(cJSON_GetStringValue(tc), &node->tc);
    node->x = number(obj, "x", node->x, -720, 1080);
    node->y = number(obj, "y", node->y, -720, 1080);
    if (node->kind == NODE_TRI) {
        node->x1 = tri_coord(obj, "x1", node->x1, GFX_WIDTH);
        node->y1 = tri_coord(obj, "y1", node->y1, GFX_HEIGHT);
        node->x2 = tri_coord(obj, "x2", node->x2, GFX_WIDTH);
        node->y2 = tri_coord(obj, "y2", node->y2, GFX_HEIGHT);
        node->x3 = tri_coord(obj, "x3", node->x3, GFX_WIDTH);
        node->y3 = tri_coord(obj, "y3", node->y3, GFX_HEIGHT);
    } else {
        node->x1 = number(obj, "x1", node->x1, -720, 1080);
        node->y1 = number(obj, "y1", node->y1, -720, 1080);
        node->x2 = number(obj, "x2", node->x2, -720, 1080);
        node->y2 = number(obj, "y2", node->y2, -720, 1080);
    }
    node->w = number(obj, "w", node->w, 0, 720);
    node->h = number(obj, "h", node->h, 0, 720);
    node->r = number(obj, "r", node->r, 0, 720);
    node->sw = number(obj, "sw", node->sw, 0, 180);
    node->a1 = number(obj, "a1", node->a1, -360, 720);
    node->a2 = number(obj, "a2", node->a2, -360, 720);
    node->fs = number(obj, "fs", node->fs, 1, 3);
    return true;
}

static bool add_nodes(scene_frame_t *frame, const cJSON *array)
{
    if (!cJSON_IsArray(array) || cJSON_GetArraySize(array) > (int)(SCENE_MAX_NODES - frame->count)) return false;
    const cJSON *item;
    cJSON_ArrayForEach(item, array) {
        const char *id = str(item, "id"), *type = str(item, "t");
        if (!valid_id(id) || !type || find(frame, id) >= 0) return false;
        scene_node_t *node = &frame->nodes[frame->count++];
        *node = (scene_node_t){ .c = gfx_rgb565(0xf8fafc), .tc = gfx_rgb565(0x1a1926),
                                .vis = true, .fill = !strcmp(type, "btn") || !strcmp(type, "tri"),
                                .sw = 2, .a2 = 360, .fs = 1,
                                .r = !strcmp(type, "btn") ? 14 : 0 };
        strcpy(node->id, id);
        if (!update(node, item)) return false;
    }
    return true;
}

static bool patch(scene_frame_t *frame, const cJSON *msg)
{
    const cJSON *del = cJSON_GetObjectItemCaseSensitive(msg, "del");
    const cJSON *set = cJSON_GetObjectItemCaseSensitive(msg, "set");
    const cJSON *add = cJSON_GetObjectItemCaseSensitive(msg, "add");
    if ((del && !cJSON_IsArray(del)) || (set && !cJSON_IsObject(set))) return false;
    const cJSON *item;
    cJSON_ArrayForEach(item, del) {
        const char *id = cJSON_GetStringValue(item);
        if (!valid_id(id)) return false;
        int i = find(frame, id);
        if (i >= 0) {
            --frame->count;
            memmove(&frame->nodes[i], &frame->nodes[i + 1], (frame->count - (unsigned)i) * sizeof(frame->nodes[0]));
        }
    }
    cJSON_ArrayForEach(item, set) {
        if (!valid_id(item->string)) return false;
        int i = find(frame, item->string);
        if (i >= 0 && !update(&frame->nodes[i], item)) return false;
    }
    return !add || add_nodes(frame, add);
}

bool scene_apply_json(scene_t *scene, const cJSON *msg)
{
    const char *op = str(msg, "op");
    const cJSON *rev = cJSON_GetObjectItemCaseSensitive(msg, "rev");
    if (!op || !cJSON_IsNumber(rev) || !(rev->valuedouble >= 0 && rev->valuedouble <= UINT32_MAX)) return false;
    uint32_t value = (uint32_t)rev->valuedouble;
    const scene_frame_t *current = &scene->frames[scene->active];
    if (value != rev->valuedouble || (scene->ready && value <= current->rev)) return false;
    scene_frame_t *next = &scene->frames[scene->active ^ 1];
    if (!strcmp(op, "scene")) {
        next->count = 0;
        next->bg = gfx_rgb565(0x16132b);
        if (!add_nodes(next, cJSON_GetObjectItemCaseSensitive(msg, "nodes"))) return false;
    } else if (!strcmp(op, "patch") && scene->ready) {
        next->count = current->count;
        next->bg = current->bg;
        memcpy(next->nodes, current->nodes, current->count * sizeof(current->nodes[0]));
        if (!patch(next, msg)) return false;
    } else {
        return false;
    }
    gfx_parse_color(str(msg, "bg"), &next->bg);
    next->rev = value;
    scene->active ^= 1;
    scene->ready = true;
    return true;
}

static void text_box(const scene_node_t *node, int *x, int *y, int *w, int *h)
{
    if (node->kind == NODE_BTN) {
        *x = node->x + 4;
        *y = node->y;
        *w = node->w - 8;
        *h = node->h;
    } else {
        *w = node->w ? node->w : GFX_WIDTH;
        *h = node->h ? node->h : 8 * node->fs;
        *x = node->x - *w / 2;
        *y = node->y - *h / 2;
    }
}

void scene_render(const scene_t *scene, uint16_t *fb)
{
    const scene_frame_t *frame = &scene->frames[scene->active];
    gfx_clear(fb, frame->bg);
    if (!scene->ready) return;
    for (unsigned i = 0; i < frame->count; ++i) {
        const scene_node_t *n = &frame->nodes[i];
        if (!n->vis) continue;
        switch (n->kind) {
        case NODE_LINE:
            gfx_thick_line(fb, n->x1, n->y1, n->x2, n->y2, n->sw, n->c);
            break;
        case NODE_TRI:
            gfx_tri(fb, n->x1, n->y1, n->x2, n->y2, n->x3, n->y3, n->sw, n->fill, n->c);
            break;
        case NODE_ARC:
            gfx_thick_arc(fb, n->x, n->y, n->r, n->a1, n->a2, n->sw, n->c);
            break;
        case NODE_CIRCLE:
            gfx_circle(fb, n->x, n->y, n->r, n->sw, n->fill, n->border && !n->fill ? n->bc : n->c);
            if (n->border && n->fill && n->sw) gfx_circle(fb, n->x, n->y, n->r, n->sw, false, n->bc);
            break;
        case NODE_RECT:
        case NODE_BTN:
            gfx_round_rect(fb, n->x, n->y, n->w, n->h, n->r, n->sw, n->fill, n->border && !n->fill ? n->bc : n->c);
            if (n->border && n->fill && n->sw) gfx_round_rect(fb, n->x, n->y, n->w, n->h, n->r, n->sw, false, n->bc);
            break;
        case NODE_TEXT:
            break;
        }
        if ((n->kind == NODE_TEXT || n->kind == NODE_BTN) && n->s[0]) {
            int x, y, w, h;
            text_box(n, &x, &y, &w, &h);
            gfx_text(fb, x, y, w, h, n->fs, n->s, n->kind == NODE_BTN || n->text_color ? n->tc : n->c);
        }
    }
}

static bool hit(const scene_node_t *n, int x, int y)
{
    int pad = 10;
    if (n->kind == NODE_CIRCLE || n->kind == NODE_ARC) {
        int dx = x - n->x, dy = y - n->y, r = n->r + pad;
        return dx * dx + dy * dy <= r * r;
    }
    if (n->kind == NODE_LINE) {
        float dx = n->x2 - n->x1, dy = n->y2 - n->y1, px = x - n->x1, py = y - n->y1;
        float length = dx * dx + dy * dy, t = length ? (px * dx + py * dy) / length : 0;
        t = t < 0 ? 0 : t > 1 ? 1 : t;
        px -= t * dx;
        py -= t * dy;
        int r = pad + n->sw / 2;
        return px * px + py * py <= r * r;
    }
    int left = n->x, top = n->y, w = n->w, h = n->h;
    if (n->kind == NODE_TRI) {
        pad += n->sw / 2;
        left = min(n->x1, min(n->x2, n->x3));
        top = min(n->y1, min(n->y2, n->y3));
        w = max(n->x1, max(n->x2, n->x3)) - left + 1;
        h = max(n->y1, max(n->y2, n->y3)) - top + 1;
    } else if (n->kind == NODE_TEXT) {
        text_box(n, &left, &top, &w, &h);
        if (!n->w) {
            w = (int)strlen(n->s) * n->fs * 8;
            left = n->x - w / 2;
        }
    }
    return w > 0 && h > 0 && x >= left - pad && x < left + w + pad && y >= top - pad && y < top + h + pad;
}

const scene_node_t *scene_hit_test(const scene_t *scene, int x, int y)
{
    if (!scene->ready || x < 0 || y < 0 || x >= GFX_WIDTH || y >= GFX_HEIGHT) return NULL;
    const scene_frame_t *frame = &scene->frames[scene->active];
    for (unsigned i = frame->count; i > 0; --i) {
        const scene_node_t *n = &frame->nodes[i - 1];
        if (n->vis && n->on && hit(n, x, y)) return n;
    }
    return NULL;
}

void scene_render_offline(uint16_t *fb, const char *character, const char *ip, const char *status)
{
    bool cake = !strcmp(character, "Cake"), mint = !strcmp(character, "Mint"), plum = !strcmp(character, "Plum");
    gfx_color_t bg = gfx_rgb565(cake ? 0xfff3d1 : mint ? 0x7be3c6 : plum ? 0xd8c7ff : 0xfcd34d);
    gfx_color_t mark = gfx_rgb565(cake ? 0xf59e0b : mint ? 0x228e81 : plum ? 0x9f7aea : 0xea580c);
    gfx_color_t ink = gfx_rgb565(0x1a1926);
    gfx_clear(fb, bg);
    gfx_circle(fb, 180, 180, 172, 3, false, gfx_rgb565(cake ? 0x8c8577 : mint ? 0x4b867a : 0x74687c));
    if (cake) {
        gfx_round_rect(fb, 147, 66, 14, 24, 7, 0, true, mark);
        gfx_round_rect(fb, 173, 61, 14, 33, 7, 0, true, mark);
        gfx_round_rect(fb, 199, 66, 14, 24, 7, 0, true, mark);
    } else if (mint) {
        gfx_round_rect(fb, 156, 72, 48, 12, 6, 0, true, mark);
    } else {
        gfx_circle(fb, 180, 78, 16, 0, true, mark);
        if (plum) gfx_circle(fb, 190, 72, 14, 0, true, bg);
    }
    gfx_round_rect(fb, 95, 123, 34, 38, 17, 0, true, ink);
    gfx_round_rect(fb, 231, 123, 34, 38, 17, 0, true, ink);
    gfx_thick_line(fb, 38, 177, 87, 188, 6, ink);
    gfx_thick_line(fb, 41, 212, 87, 199, 6, ink);
    gfx_thick_line(fb, 273, 188, 322, 177, 6, ink);
    gfx_thick_line(fb, 273, 199, 319, 212, 6, ink);
    gfx_round_rect(fb, 165, 163, 30, 17, 8, 0, true, ink);
    gfx_thick_arc(fb, 162, 193, 18, 0, 165, 7, ink);
    gfx_thick_arc(fb, 198, 193, 18, 15, 180, 7, ink);
    gfx_round_rect(fb, 72, 244, 216, 34, 17, 0, true, gfx_rgb565(0xfffaee));
    gfx_round_rect(fb, 72, 244, 216, 34, 17, 2, false, ink);
    gfx_text(fb, 78, 244, 204, 34, 1, status, ink);
    gfx_text(fb, 75, 285, 210, 24, 1, ip, ink);
}
