#include <assert.h>
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

int main(void)
{
    guarded[0] = guarded[GFX_PIXELS + 1] = 0x77aa;
    primitives();
    scenes();
    scene_render_offline(fb, "Cake", "203.0.113.1", "Waiting for bridge");
    assert(pixel(172, 100) == gfx_rgb565(0xfff3d1));
    assert(pixel(180, 77) == gfx_rgb565(0xf59e0b));
    assert(pixel(112, 142) == gfx_rgb565(0x1a1926));
    scene_render_offline(fb, "Mint", "", "Joining Wi-Fi");
    assert(pixel(172, 100) == gfx_rgb565(0x7be3c6));
    assert(pixel(180, 77) == gfx_rgb565(0x228e81));
    assert(pixel(248, 142) == gfx_rgb565(0x1a1926));
    assert(guarded[0] == 0x77aa && guarded[GFX_PIXELS + 1] == 0x77aa);
    puts("PASS RGB565 byte order, clipped primitives, text wrapping, keyed scene patches, hit tests, and offline faces");
}
