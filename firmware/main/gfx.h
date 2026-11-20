#pragma once

#include <stdbool.h>
#include <stdint.h>

enum { GFX_WIDTH = 360, GFX_HEIGHT = 360, GFX_PIXELS = GFX_WIDTH * GFX_HEIGHT };
typedef uint16_t gfx_color_t;

gfx_color_t gfx_rgb565(uint32_t rgb);
bool gfx_parse_color(const char *hex, gfx_color_t *out);
void gfx_clear(uint16_t *fb, gfx_color_t color);
void gfx_circle(uint16_t *fb, int x, int y, int r, int sw, bool fill, gfx_color_t color);
void gfx_round_rect(uint16_t *fb, int x, int y, int w, int h, int r, int sw, bool fill, gfx_color_t color);
void gfx_thick_line(uint16_t *fb, int x1, int y1, int x2, int y2, int sw, gfx_color_t color);
void gfx_tri(uint16_t *fb, int x1, int y1, int x2, int y2, int x3, int y3, int sw, bool fill, gfx_color_t color);
void gfx_thick_arc(uint16_t *fb, int x, int y, int r, int a1, int a2, int sw, gfx_color_t color);
void gfx_text(uint16_t *fb, int x, int y, int w, int h, int fs, const char *text, gfx_color_t color);
