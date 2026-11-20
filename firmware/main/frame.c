#include "frame.h"

#include <string.h>

void frame_union(frame_bounds_t *dirty, frame_rect_t r)
{
    if (!r.w || !r.h || r.x + r.w > FRAME_WIDTH || r.y + r.h > FRAME_HEIGHT) return;
    // Neither panel requires even coordinates, but an even RGB565 width keeps
    // all SPI DMA strips word-sized, including the final single-row strip.
    _Static_assert(FRAME_WIDTH % 2 == 0, "LCD width must fit aligned RGB565 strips");
    int x0 = r.x & ~1, x1 = (r.x + r.w + 1) & ~1;
    int y0 = r.y, y1 = r.y + r.h;
    bool empty = dirty->x0 == dirty->x1;
    if (empty || x0 < dirty->x0) dirty->x0 = x0;
    if (empty || y0 < dirty->y0) dirty->y0 = y0;
    if (empty || x1 > dirty->x1) dirty->x1 = x1;
    if (empty || y1 > dirty->y1) dirty->y1 = y1;
}

static uint16_t le16(const uint8_t *p)
{
    return (uint16_t)p[0] | ((uint16_t)p[1] << 8);
}

static bool rle_valid(const uint8_t *p, size_t len, size_t pixels)
{
    while (pixels && len) {
        uint8_t tag = *p++;
        --len;
        size_t run = (tag & 0x7f) + 1;
        size_t bytes = (tag & 0x80) ? 2 : run * 2;
        if (run > pixels || bytes > len) return false;
        p += bytes;
        len -= bytes;
        pixels -= run;
    }
    return pixels == 0 && len == 0;
}

static void decode_rle(uint16_t *fb, const uint8_t *p, frame_rect_t r)
{
    size_t row = 0, col = 0;
    while (row < r.h) {
        uint8_t tag = *p++;
        size_t run = (tag & 0x7f) + 1;
        bool repeat = tag & 0x80;
        uint16_t pixel;
        if (repeat) {
            // Copy the representation, not a numeric RGB565 value.
            memcpy(&pixel, p, 2);
            p += 2;
        }
        while (run) {
            size_t span = r.w - col;
            if (span > run) span = run;
            uint16_t *out = fb + (r.y + row) * FRAME_WIDTH + r.x + col;
            if (repeat) {
                for (size_t i = 0; i < span; ++i) out[i] = pixel;
            } else {
                memcpy(out, p, span * 2);
                p += span * 2;
            }
            run -= span;
            col += span;
            if (col == r.w) {
                col = 0;
                ++row;
            }
        }
    }
}

bool frame_apply(uint16_t *fb, const uint8_t *data, size_t len, frame_rect_t *dirty)
{
    if (!fb || !data || !dirty || len < FRAME_HEADER_SIZE || data[0] != FRAME_TAG) return false;
    frame_rect_t r = {
        .x = le16(data + 1), .y = le16(data + 3), .w = le16(data + 5),
        .h = le16(data + 7), .rev_lo = le16(data + 10),
    };
    if (!r.w || !r.h || r.x + r.w > FRAME_WIDTH || r.y + r.h > FRAME_HEIGHT) return false;
    const uint8_t *payload = data + FRAME_HEADER_SIZE;
    len -= FRAME_HEADER_SIZE;
    size_t pixels = (size_t)r.w * r.h;
    if (data[9] == 0) {
        if (len != pixels * 2) return false;
        for (size_t row = 0; row < r.h; ++row) {
            memcpy(fb + (r.y + row) * FRAME_WIDTH + r.x, payload + row * r.w * 2, r.w * 2);
        }
    } else if (data[9] == 1) {
        // Validate the whole stream before exposing any pixels to a later flush.
        if (!rle_valid(payload, len, pixels)) return false;
        decode_rle(fb, payload, r);
    } else {
        return false;
    }
    *dirty = r;
    return true;
}
