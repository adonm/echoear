#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

enum { FRAME_WIDTH = 360, FRAME_HEIGHT = 360, FRAME_PIXELS = FRAME_WIDTH * FRAME_HEIGHT };
enum { FRAME_TAG = 0x03, FRAME_HEADER_SIZE = 12, FRAME_PACKET_CAP = 260 * 1024 };

typedef struct {
    uint16_t x, y, w, h, rev_lo;
} frame_rect_t;

// Pixels are stored in LCD wire byte order. Invalid packets leave fb and dirty untouched.
bool frame_apply(uint16_t *fb, const uint8_t *data, size_t len, frame_rect_t *dirty);
