#include <assert.h>
#include <limits.h>
#include <stdio.h>
#include <string.h>

#include "gfx.h"
#include "scene.h"

static uint16_t guarded[GFX_PIXELS + 2];
static uint16_t *const fb = guarded + 1;
static scene_t scene;

static gfx_color_t pixel(int x, int y)
{
    return fb[y * GFX_WIDTH + x];
}

static bool apply(const char *text)
{
    cJSON *object = cJSON_Parse(text);
    assert(object);
    bool ok = scene_apply_json(&scene, object);
    cJSON_Delete(object);
    return ok;
}

static void hit(int x, int y, const char *id)
{
    const scene_node_t *node = scene_hit_test(&scene, x, y);
    assert(id ? node && !strcmp(node->id, id) : !node);
}

static void primitives(void)
{
    const gfx_color_t red = gfx_rgb565(0xff0000), green = gfx_rgb565(0x00ff00), blue = gfx_rgb565(0x0000ff);
    assert(red == 0x00f8 && green == 0xe007 && blue == 0x1f00);
    const unsigned char *wire = (const unsigned char *)&red;
    assert(wire[0] == 0xf8 && wire[1] == 0);
    gfx_color_t parsed;
    assert(gfx_parse_color("#fF0000", &parsed) && parsed == red);
    assert(!gfx_parse_color("#fff", &parsed) && !gfx_parse_color("#0000000", &parsed));
    assert(!gfx_parse_color("#00XZ00", &parsed));
    gfx_clear(fb, 0);
    gfx_circle(fb, 0, 0, 18, 0, true, red);
    assert(pixel(0, 0) == red && pixel(18, 18) == 0);
    gfx_circle(fb, 80, 80, 12, 3, false, blue);
    assert(pixel(80, 80) == 0 && pixel(91, 80) == blue);
    gfx_round_rect(fb, 20, 30, 40, 30, 8, 2, false, green);
    assert(pixel(20, 30) == 0 && pixel(40, 30) == green && pixel(40, 45) == 0);
    gfx_thick_line(fb, -5, 200, 20, 200, 6, red);
    assert(pixel(0, 200) == red && pixel(22, 200) == red && pixel(24, 200) == 0);
    gfx_thick_arc(fb, 100, 100, 20, 0, 90, 4, green);
    assert(pixel(120, 100) == green && pixel(100, 120) == green);
    assert(pixel(100, 80) == 0 && pixel(80, 100) == 0);
    gfx_text(fb, 120, 150, 16, 40, 1, "I I I", red);
    assert(pixel(127, 156) == red && pixel(127, 166) == red && pixel(127, 176) == red);
    gfx_round_rect(fb, 359, 359, 50, 350, 0, 0, true, blue);
    assert(pixel(359, 359) == blue);
}

static void triangles(void)
{
    const gfx_color_t ink = gfx_rgb565(0x38bdf8);
    gfx_clear(fb, 0);
    gfx_tri(fb, 150, 180, 180, 120, 210, 180, 3, true, ink);
    assert(pixel(180, 120) == ink && pixel(180, 150) == ink);
    assert(pixel(179, 121) == 0 && pixel(180, 121) == ink && pixel(181, 121) == 0);
    assert(pixel(149, 180) == 0 && pixel(150, 180) == ink && pixel(210, 180) == ink && pixel(211, 180) == 0);
    assert(pixel(180, 181) == 0);

    gfx_clear(fb, 0);
    gfx_tri(fb, 180, 120, 150, 180, 210, 180, 0, false, ink);
    assert(pixel(165, 150) == 0 && pixel(180, 180) == 0);
    gfx_tri(fb, 180, 120, 150, 180, 210, 180, 6, false, ink);
    assert(pixel(165, 150) == ink && pixel(195, 150) == ink && pixel(180, 180) == ink);
    assert(pixel(180, 150) == 0 && pixel(180, 183) == ink && pixel(180, 184) == 0);

    gfx_clear(fb, 0);
    gfx_tri(fb, -20, 180, 180, -20, 380, 180, 3, true, ink);
    for (int y = 0; y < GFX_HEIGHT; ++y) {
        for (int x = 0; x < GFX_WIDTH; ++x) {
            int dx = x - 180, dy = y - 180;
            bool inside = y <= 180 && x + y >= 160 && x - y <= 200 && dx * dx + dy * dy <= 180 * 180;
            assert(pixel(x, y) == (inside ? ink : 0));
        }
    }

    gfx_clear(fb, 0);
    gfx_tri(fb, -20, 0, 380, 0, 180, 380, 3, false, ink);
    assert(pixel(180, 0) == ink && pixel(179, 0) == 0 && pixel(181, 0) == 0);

    gfx_clear(fb, 0);
    gfx_tri(fb, INT_MIN, INT_MIN, INT_MAX, INT_MIN, INT_MAX, INT_MAX, 3, true, ink);
    assert(pixel(179, 180) == 0 && pixel(180, 180) == ink && pixel(181, 180) == ink);
    gfx_clear(fb, 0);
    gfx_tri(fb, INT_MIN, 180, INT_MAX, 180, INT_MAX, INT_MIN, 3, false, ink);
    assert(pixel(0, 180) == ink && pixel(359, 180) == ink && pixel(180, 182) == 0);
}

static void scenes(void)
{
    assert(apply("{\"op\":\"scene\",\"rev\":5,\"bg\":\"#0000FF\",\"nodes\":["
                 "{\"id\":\"base\",\"t\":\"rect\",\"x\":20,\"y\":20,\"w\":60,\"h\":55,\"fill\":true,\"c\":\"#FF0000\",\"on\":\"tap\"},"
                 "{\"id\":\"top\",\"t\":\"btn\",\"x\":26,\"y\":26,\"w\":32,\"h\":30,\"r\":0,\"on\":\"tap\",\"c\":\"#00FF00\"}]}"));
    scene_render(&scene, fb);
    assert(pixel(5, 5) == gfx_rgb565(0x0000ff) && pixel(40, 40) == gfx_rgb565(0x00ff00));
    hit(35, 35, "top");
    hit(19, 30, "top"); // Six pixels outside top still hits it in reverse draw order.
    hit(10, 20, "base");
    hit(9, 20, NULL);
    assert(apply("{\"op\":\"patch\",\"rev\":6,\"bg\":\"#000000\",\"set\":{\"top\":{\"vis\":false},"
                 "\"base\":{\"c\":\"#FFFFFF\"}},\"add\":[{\"id\":\"line\",\"t\":\"line\","
                 "\"x1\":10,\"y1\":100,\"x2\":80,\"y2\":100,\"sw\":5,\"on\":\"drag\",\"c\":\"#FCBC00\"}]}"));
    scene_render(&scene, fb);
    assert(pixel(5, 5) == 0 && pixel(40, 40) == gfx_rgb565(0xffffff));
    assert(pixel(20, 100) == gfx_rgb565(0xfcbc00));
    hit(35, 35, "base");
    hit(45, 111, "line");
    hit(45, 113, NULL);
    assert(!apply("{\"op\":\"patch\",\"rev\":7,\"del\":[\"base\"],\"add\":[{\"id\":\"top\",\"t\":\"arc\"}]}"));
    hit(35, 35, "base"); // Invalid patch is atomic.
    assert(apply("{\"op\":\"patch\",\"rev\":7,\"del\":[\"base\",\"line\"],\"set\":{\"top\":{\"vis\":true,\"x\":150}}}"));
    hit(35, 35, NULL);
    hit(155, 30, "top");
    assert(!apply("{\"op\":\"patch\",\"rev\":6,\"del\":[\"top\"]}"));
    hit(155, 30, "top");
}

static void triangle_scene(void)
{
    const gfx_color_t blue = gfx_rgb565(0x38bdf8), red = gfx_rgb565(0xff0000);
    assert(apply("{\"op\":\"scene\",\"rev\":8,\"bg\":\"#000000\",\"nodes\":["
                 "{\"id\":\"fin\",\"t\":\"tri\",\"x1\":180,\"y1\":120,\"x2\":150,\"y2\":180,"
                 "\"x3\":210,\"y3\":180,\"c\":\"#38BDF8\",\"fill\":true,\"sw\":3,\"on\":\"tap\"},"
                 "{\"id\":\"default\",\"t\":\"tri\",\"x1\":-10,\"y1\":170,\"x2\":60,\"y2\":160,"
                 "\"x3\":50,\"y3\":220,\"c\":\"#FF0000\",\"on\":\"tap\"}]}"));
    scene_render(&scene, fb);
    assert(pixel(180, 150) == blue && pixel(45, 180) == red);
    const scene_node_t *n = scene_hit_test(&scene, 45, 180);
    assert(n && n->kind == NODE_TRI && n->x1 == 0 && n->fill && n->sw == 2);
    hit(139, 109, "fin"); // Padded box, not a point-in-triangle test.
    hit(138, 109, NULL);
    hit(221, 191, "fin");
    hit(222, 191, NULL);

    assert(apply("{\"op\":\"patch\",\"rev\":9,\"set\":{\"fin\":{\"x1\":240,\"y1\":-100,"
                 "\"x2\":220,\"y2\":100,\"x3\":900,\"y3\":78,\"fill\":false,\"sw\":6,"
                 "\"c\":\"#FF0000\",\"on\":\"drag\"}}}"));
    scene_render(&scene, fb);
    assert(pixel(180, 150) == 0 && pixel(220, 103) == red && pixel(220, 104) == 0);
    hit(180, 150, NULL);
    n = scene_hit_test(&scene, 218, 78);
    assert(n && !strcmp(n->id, "fin") && n->on == NODE_DRAG && !n->fill && n->sw == 6);
    assert(n->x1 == 240 && n->y1 == 0 && n->x2 == 220 && n->y2 == 100 && n->x3 == 359 && n->y3 == 78);

    assert(apply("{\"op\":\"patch\",\"rev\":10,\"set\":{\"fin\":{\"vis\":false}}}"));
    scene_render(&scene, fb);
    assert(pixel(220, 103) == 0);
    hit(218, 78, NULL);
    assert(apply("{\"op\":\"patch\",\"rev\":11,\"set\":{\"fin\":{\"vis\":true,\"on\":\"\"}}}"));
    scene_render(&scene, fb);
    assert(pixel(220, 103) == red);
    hit(218, 78, NULL);
}

int main(void)
{
    guarded[0] = guarded[GFX_PIXELS + 1] = 0x77aa;
    primitives();
    triangles();
    scenes();
    triangle_scene();
    scene_render_offline(fb, "Cake", "203.0.113.1", "Waiting for bridge");
    assert(pixel(172, 100) == gfx_rgb565(0xfff3d1));
    assert(pixel(180, 77) == gfx_rgb565(0xf59e0b));
    assert(pixel(112, 142) == gfx_rgb565(0x1a1926));
    scene_render_offline(fb, "Mint", "", "Joining Wi-Fi");
    assert(pixel(172, 100) == gfx_rgb565(0x7be3c6));
    assert(pixel(180, 77) == gfx_rgb565(0x228e81));
    assert(pixel(248, 142) == gfx_rgb565(0x1a1926));
    assert(guarded[0] == 0x77aa && guarded[GFX_PIXELS + 1] == 0x77aa);
    puts("PASS RGB565 byte order, clipped primitives, filled/stroked triangles, scene patches, hit tests, and offline faces");
}
