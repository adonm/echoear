#include "gfx.h"

#include <string.h>

#include "frame.h"

enum { ART_SIZE = 360, SIDE = FRAME_WIDTH < FRAME_HEIGHT ? FRAME_WIDTH : FRAME_HEIGHT,
       LEFT = (FRAME_WIDTH - SIDE) / 2, TOP = (FRAME_HEIGHT - SIDE) / 2 };

static uint16_t wire_color(uint32_t rgb)
{
    uint16_t value = ((rgb >> 8) & 0xf800) | ((rgb >> 5) & 0x07e0) | ((rgb >> 3) & 0x001f);
    const uint8_t bytes[] = { value >> 8, value & 0xff };
    uint16_t pixel;
    memcpy(&pixel, bytes, sizeof(pixel));
    return pixel;
}

static void rect(uint16_t *fb, int x, int y, int w, int h, uint16_t color)
{
    for (int row = y; row < y + h; ++row) {
        for (int col = x; col < x + w; ++col) fb[row * FRAME_WIDTH + col] = color;
    }
}

static void art_rect(uint16_t *fb, int x, int y, int w, int h, uint16_t color)
{
    int l = x * SIDE / ART_SIZE, t = y * SIDE / ART_SIZE;
    rect(fb, LEFT + l, TOP + t, (x + w) * SIDE / ART_SIZE - l, (y + h) * SIDE / ART_SIZE - t, color);
}

// All offline shapes are placed entirely inside the framebuffer.
static void circle(uint16_t *fb, int x, int y, int r, int inner, bool lower_half, uint16_t color)
{
    x = LEFT + x * SIDE / ART_SIZE;
    y = TOP + y * SIDE / ART_SIZE;
    r = r * SIDE / ART_SIZE;
    inner = inner * SIDE / ART_SIZE;
    for (int dy = lower_half ? 0 : -r; dy <= r; ++dy) {
        for (int dx = -r; dx <= r; ++dx) {
            int d2 = dx * dx + dy * dy;
            if (d2 <= r * r && d2 >= inner * inner) fb[(y + dy) * FRAME_WIDTH + x + dx] = color;
        }
    }
}

void gfx_offline_cat(uint16_t *fb)
{
    uint16_t bg = wire_color(0xfff3d1);
    uint16_t mark = wire_color(0xf59e0b);
    uint16_t ink = wire_color(0x1a1926);
    rect(fb, 0, 0, FRAME_WIDTH, FRAME_HEIGHT, bg);
    circle(fb, 180, 180, 172, 169, false, wire_color(0x8c8577));
    art_rect(fb, 147, 67, 14, 24, mark);
    art_rect(fb, 173, 61, 14, 33, mark);
    art_rect(fb, 199, 67, 14, 24, mark);
    circle(fb, 112, 142, 17, 0, false, ink);
    circle(fb, 248, 142, 17, 0, false, ink);
    for (int y = 180; y <= 205; y += 25) {
        art_rect(fb, 40, y, 47, 6, ink);
        art_rect(fb, 273, y, 47, 6, ink);
    }
    circle(fb, 180, 172, 11, 0, false, ink);
    circle(fb, 162, 191, 18, 13, true, ink);
    circle(fb, 198, 191, 18, 13, true, ink);
}
