#include "gfx.h"

#include <string.h>

#include "frame.h"

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

// All offline shapes are placed entirely inside the framebuffer.
static void circle(uint16_t *fb, int x, int y, int r, int inner, bool lower_half, uint16_t color)
{
    for (int dy = lower_half ? 0 : -r; dy <= r; ++dy) {
        for (int dx = -r; dx <= r; ++dx) {
            int d2 = dx * dx + dy * dy;
            if (d2 <= r * r && d2 >= inner * inner) fb[(y + dy) * FRAME_WIDTH + x + dx] = color;
        }
    }
}

void gfx_offline_cat(uint16_t *fb, const char *character)
{
    bool cake = !strcmp(character, "Cake"), mint = !strcmp(character, "Mint"), plum = !strcmp(character, "Plum");
    uint16_t bg = wire_color(cake ? 0xfff3d1 : mint ? 0x7be3c6 : plum ? 0xd8c7ff : 0xfcd34d);
    uint16_t mark = wire_color(cake ? 0xf59e0b : mint ? 0x228e81 : plum ? 0x9f7aea : 0xea580c);
    uint16_t ink = wire_color(0x1a1926);
    rect(fb, 0, 0, FRAME_WIDTH, FRAME_HEIGHT, bg);
    circle(fb, 180, 180, 172, 169, false, wire_color(cake ? 0x8c8577 : mint ? 0x4b867a : 0x74687c));
    if (cake) {
        rect(fb, 147, 67, 14, 24, mark);
        rect(fb, 173, 61, 14, 33, mark);
        rect(fb, 199, 67, 14, 24, mark);
    } else if (mint) {
        rect(fb, 156, 72, 48, 12, mark);
    } else {
        circle(fb, 180, 78, 16, 0, false, mark);
        if (plum) circle(fb, 190, 72, 14, 0, false, bg);
    }
    circle(fb, 112, 142, 17, 0, false, ink);
    circle(fb, 248, 142, 17, 0, false, ink);
    for (int y = 180; y <= 205; y += 25) {
        rect(fb, 40, y, 47, 6, ink);
        rect(fb, 273, y, 47, 6, ink);
    }
    circle(fb, 180, 172, 11, 0, false, ink);
    circle(fb, 162, 191, 18, 13, true, ink);
    circle(fb, 198, 191, 18, 13, true, ink);
}
