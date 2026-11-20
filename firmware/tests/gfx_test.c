#include "frame.h"
#include "gfx.h"

#include <assert.h>
#include <stdio.h>

static uint16_t fb[FRAME_PIXELS];

static void expect_pixel(unsigned x, unsigned y, uint8_t hi, uint8_t lo)
{
    const uint8_t *bytes = (const uint8_t *)&fb[y * FRAME_WIDTH + x];
    assert(bytes[0] == hi && bytes[1] == lo);
}

int main(void)
{
    gfx_offline_cat(fb, "Cake");
    expect_pixel(0, 0, 0xff, 0x9a);
    expect_pixel(359, 359, 0xff, 0x9a);
    expect_pixel(180, 77, 0xf4, 0xe1);
    expect_pixel(112, 142, 0x18, 0xc4);
    expect_pixel(162, 207, 0x18, 0xc4);
    gfx_offline_cat(fb, "Mint");
    expect_pixel(0, 0, 0x7f, 0x18);
    expect_pixel(180, 77, 0x24, 0x70);
    expect_pixel(248, 142, 0x18, 0xc4);
    puts("PASS offline cat fallback in LCD byte order");
}
