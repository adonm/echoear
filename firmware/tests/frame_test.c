#include "frame.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static uint16_t fb[FRAME_PIXELS];
static uint8_t raw[FRAME_HEADER_SIZE + FRAME_PIXELS * 2];
enum { TEST_REV = 0xab3e, SENTINEL = 0xa5 };

static void header(uint8_t *packet, unsigned x, unsigned y, unsigned w, unsigned h, uint8_t codec)
{
    const uint8_t bytes[] = {
        FRAME_TAG, x & 0xff, x >> 8, y & 0xff, y >> 8,
        w & 0xff, w >> 8, h & 0xff, h >> 8, codec, TEST_REV & 0xff, TEST_REV >> 8,
    };
    memcpy(packet, bytes, sizeof(bytes));
}

static void apply(const uint8_t *data, size_t len, unsigned x, unsigned y, unsigned w, unsigned h)
{
    frame_rect_t dirty;
    assert(frame_apply(fb, data, len, &dirty));
    assert(dirty.x == x && dirty.y == y && dirty.w == w && dirty.h == h && dirty.rev_lo == TEST_REV);
}

static void expect_rect(unsigned x, unsigned y, unsigned w, unsigned h, const uint8_t *pixels)
{
    const uint8_t *bytes = (const uint8_t *)fb;
    for (unsigned row = 0; row < FRAME_HEIGHT; ++row) {
        for (unsigned col = 0; col < FRAME_WIDTH; ++col) {
            const uint8_t *actual = bytes + (row * FRAME_WIDTH + col) * 2;
            if (row >= y && row < y + h && col >= x && col < x + w) {
                assert(memcmp(actual, pixels + ((row - y) * w + col - x) * 2, 2) == 0);
            } else {
                assert(actual[0] == SENTINEL && actual[1] == SENTINEL);
            }
        }
    }
}

static void test_raw(void)
{
    header(raw, 0, 0, FRAME_WIDTH, FRAME_HEIGHT, 0);
    for (unsigned i = 0; i < FRAME_PIXELS; ++i) {
        raw[FRAME_HEADER_SIZE + i * 2] = (uint8_t)(i >> 8);
        raw[FRAME_HEADER_SIZE + i * 2 + 1] = (uint8_t)i;
    }
    apply(raw, sizeof(raw), 0, 0, FRAME_WIDTH, FRAME_HEIGHT);
    assert(memcmp(fb, raw + FRAME_HEADER_SIZE, sizeof(fb)) == 0);

    const uint8_t pixels[] = { 0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f, 0xde, 0xad, 0xbe, 0xef, 0xff, 0xff };
    uint8_t packet[FRAME_HEADER_SIZE + sizeof(pixels)];
    header(packet, FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2, 0);
    memcpy(packet + FRAME_HEADER_SIZE, pixels, sizeof(pixels));
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, sizeof(packet), FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2);
    expect_rect(FRAME_WIDTH - 3, FRAME_HEIGHT - 2, 3, 2, pixels);
}

static void test_rle(void)
{
    const uint8_t literal[] = { 0x02, 0xf8, 0x00, 0x07, 0xe0, 0x00, 0x1f };
    uint8_t packet[FRAME_HEADER_SIZE + 1 + 128 * 2 + 3 + 3 + 3];
    header(packet, 10, 12, 1, 3, 1);
    memcpy(packet + FRAME_HEADER_SIZE, literal, sizeof(literal));
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, FRAME_HEADER_SIZE + sizeof(literal), 10, 12, 1, 3);
    expect_rect(10, 12, 1, 3, literal + 1);

    const uint8_t repeat[] = { 0x82, 0x00, 0x1f };
    const uint8_t blues[] = { 0x00, 0x1f, 0x00, 0x1f, 0x00, 0x1f };
    memcpy(packet + FRAME_HEADER_SIZE, repeat, sizeof(repeat));
    apply(packet, FRAME_HEADER_SIZE + sizeof(repeat), 10, 12, 1, 3);
    expect_rect(10, 12, 1, 3, blues);

    // Both maximum-length runs cross non-contiguous rows near the framebuffer edge.
    header(packet, FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6, 1);
    uint8_t pixels[43 * 6 * 2];
    for (unsigned i = 0; i < 128; ++i) {
        pixels[i * 2] = (uint8_t)(255 - i);
        pixels[i * 2 + 1] = (uint8_t)i;
        pixels[256 + i * 2] = 0xf8;
        pixels[256 + i * 2 + 1] = 0;
    }
    packet[FRAME_HEADER_SIZE] = 0x7f;
    memcpy(packet + FRAME_HEADER_SIZE + 1, pixels, 256);
    const uint8_t tail[] = { 0xff, 0xf8, 0, 0x00, 0x07, 0xe0, 0x80, 0x00, 0x1f };
    memcpy(packet + FRAME_HEADER_SIZE + 257, tail, sizeof(tail));
    memcpy(pixels + 512, tail + 4, 2);
    memcpy(pixels + 514, tail + 7, 2);
    memset(fb, SENTINEL, sizeof(fb));
    apply(packet, sizeof(packet), FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6);
    expect_rect(FRAME_WIDTH - 43, FRAME_HEIGHT - 6, 43, 6, pixels);
}

static void reject(const uint8_t *data, size_t len)
{
    // Allocate exactly the reported length so ASan can catch physical overreads too.
    uint8_t *packet = malloc(len ? len : 1);
    assert(packet);
    memcpy(packet, data, len);
    frame_rect_t dirty = { .x = 1, .y = 2, .w = 3, .h = 4, .rev_lo = 5 };
    assert(!frame_apply(fb, packet, len, &dirty));
    assert(dirty.x == 1 && dirty.y == 2 && dirty.w == 3 && dirty.h == 4 && dirty.rev_lo == 5);
    expect_rect(0, 0, 0, 0, NULL);
    free(packet);
}

static void test_malformed(void)
{
    uint8_t packet[FRAME_HEADER_SIZE + 9] = { 0 };
    memset(fb, SENTINEL, sizeof(fb));
    const struct { unsigned x, y, w, h; uint8_t codec; } invalid[] = {
        { FRAME_WIDTH, 0, 1, 1, 0 }, { 0, FRAME_HEIGHT, 1, 1, 0 },
        { 0, 0, 0, 1, 0 }, { 0, 0, 1, 0, 0 },
        { FRAME_WIDTH - 1, 0, 2, 1, 1 }, { 0, FRAME_HEIGHT - 1, 1, 2, 1 },
        { 0, 0, FRAME_WIDTH + 1, 1, 0 }, { 0, 0, 1, FRAME_HEIGHT + 1, 0 },
        { 65535, 65535, 65535, 65535, 0 }, { 0, 0, 1, 1, 2 },
    };
    for (size_t i = 0; i < sizeof(invalid) / sizeof(invalid[0]); ++i) {
        header(packet, invalid[i].x, invalid[i].y, invalid[i].w, invalid[i].h, invalid[i].codec);
        reject(packet, FRAME_HEADER_SIZE + 2);
    }

    header(packet, FRAME_WIDTH - 1, FRAME_HEIGHT - 1, 1, 1, 0);
    packet[0] = 0x02;
    reject(packet, FRAME_HEADER_SIZE + 2);
    packet[0] = FRAME_TAG;
    reject(packet, FRAME_HEADER_SIZE + 1);
    reject(packet, FRAME_HEADER_SIZE + 3);

    header(packet, FRAME_WIDTH - 2, FRAME_HEIGHT - 2, 2, 2, 1);
    uint8_t mixed[] = { 0x01, 0x12, 0x34, 0x56, 0x78, 0x81, 0x9a, 0xbc };
    memcpy(packet + FRAME_HEADER_SIZE, mixed, sizeof(mixed));
    for (size_t len = 0; len < FRAME_HEADER_SIZE + sizeof(mixed); ++len) reject(packet, len);
    reject(packet, sizeof(packet)); // trailing byte
    packet[FRAME_HEADER_SIZE + 5] = 0x82;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // repeat overruns rectangle after literal run
    packet[FRAME_HEADER_SIZE + 5] = 0x80;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // complete runs underfill rectangle
    packet[FRAME_HEADER_SIZE] = 0x7f;
    reject(packet, FRAME_HEADER_SIZE + sizeof(mixed)); // overlarge literal run
    packet[FRAME_HEADER_SIZE] = 0xff;
    reject(packet, FRAME_HEADER_SIZE + 3); // overlarge repeat at framebuffer edge

    frame_rect_t dirty;
    assert(!frame_apply(NULL, packet, sizeof(packet), &dirty));
    assert(!frame_apply(fb, NULL, sizeof(packet), &dirty));
    assert(!frame_apply(fb, packet, sizeof(packet), NULL));
}

int main(void)
{
    test_raw();
    puts("PASS raw RGB565 full frame and sub-rect, exact wire bytes");
    test_rle();
    puts("PASS RLE565 literal, repeat, mixed and 128-pixel runs, sub-rect isolation");
    test_malformed();
    puts("PASS malformed frames rejected without framebuffer or dirty-rect changes");
}
