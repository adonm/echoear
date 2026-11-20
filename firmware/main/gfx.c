#include "gfx.h"

#include <math.h>
#include <stddef.h>

#include "font.h"

static int min(int a, int b) { return a < b ? a : b; }
static int max(int a, int b) { return a > b ? a : b; }
static int clamp(int n, int lo, int hi) { return min(hi, max(lo, n)); }

gfx_color_t gfx_rgb565(uint32_t rgb)
{
    uint16_t value = ((rgb >> 8) & 0xf800) | ((rgb >> 5) & 0x07e0) | ((rgb >> 3) & 0x001f);
    // The BSP's ST77916 QSPI path transmits the bytes unchanged, high byte first.
    return (uint16_t)((value << 8) | (value >> 8));
}

bool gfx_parse_color(const char *hex, gfx_color_t *out)
{
    if (!hex || *hex++ != '#') return false;
    uint32_t rgb = 0;
    for (int i = 0; i < 6; ++i, ++hex) {
        int digit = *hex >= '0' && *hex <= '9' ? *hex - '0' :
                    *hex >= 'a' && *hex <= 'f' ? *hex - 'a' + 10 :
                    *hex >= 'A' && *hex <= 'F' ? *hex - 'A' + 10 : -1;
        if (digit < 0) return false;
        rgb = (rgb << 4) | (uint32_t)digit;
    }
    if (*hex) return false;
    *out = gfx_rgb565(rgb);
    return true;
}

static void slab(uint16_t *fb, int x, int y, int w, int h, gfx_color_t color)
{
    for (int row = max(y, 0); row < min(y + h, GFX_HEIGHT); ++row) {
        for (int col = max(x, 0); col < min(x + w, GFX_WIDTH); ++col) {
            fb[row * GFX_WIDTH + col] = color;
        }
    }
}

void gfx_clear(uint16_t *fb, gfx_color_t color)
{
    for (size_t i = 0; i < GFX_PIXELS; ++i) fb[i] = color;
}

void gfx_circle(uint16_t *fb, int x, int y, int r, int sw, bool fill, gfx_color_t color)
{
    if (r <= 0) return;
    int inner = r - max(sw, 1);
    for (int row = max(y - r, 0); row <= min(y + r, GFX_HEIGHT - 1); ++row) {
        int dy = row - y;
        for (int col = max(x - r, 0); col <= min(x + r, GFX_WIDTH - 1); ++col) {
            int dx = col - x, d2 = dx * dx + dy * dy;
            if (d2 <= r * r && (fill || inner <= 0 || d2 >= inner * inner)) {
                fb[row * GFX_WIDTH + col] = color;
            }
        }
    }
}

static bool rounded(int x, int y, int w, int h, int r)
{
    if (x < 0 || x >= w || y < 0 || y >= h) return false;
    if (r <= 0) return true;
    int dx = max(r - 1 - x, x - (w - r)), dy = max(r - 1 - y, y - (h - r));
    dx = max(dx, 0);
    dy = max(dy, 0);
    return dx * dx + dy * dy < r * r;
}

void gfx_round_rect(uint16_t *fb, int x, int y, int w, int h, int r, int sw, bool fill, gfx_color_t color)
{
    if (w <= 0 || h <= 0) return;
    r = clamp(r, 0, min(w, h) / 2);
    if (fill && !r) {
        slab(fb, x, y, w, h, color);
        return;
    }
    int inset = max(1, sw);
    for (int row = max(y, 0); row < min(y + h, GFX_HEIGHT); ++row) {
        for (int col = max(x, 0); col < min(x + w, GFX_WIDTH); ++col) {
            int dx = col - x, dy = row - y;
            if (rounded(dx, dy, w, h, r) &&
                (fill || !rounded(dx - inset, dy - inset, w - 2 * inset, h - 2 * inset, r - inset))) {
                fb[row * GFX_WIDTH + col] = color;
            }
        }
    }
}

void gfx_thick_line(uint16_t *fb, int x1, int y1, int x2, int y2, int sw, gfx_color_t color)
{
    float r = max(sw, 1) * 0.5f, r2 = r * r;
    int pad = (int)ceilf(r);
    float dx = x2 - x1, dy = y2 - y1, length2 = dx * dx + dy * dy;
    float mul = length2 > 0 ? 1.0f / length2 : 0.0f;
    for (int y = max(min(y1, y2) - pad, 0); y <= min(max(y1, y2) + pad, GFX_HEIGHT - 1); ++y) {
        for (int x = max(min(x1, x2) - pad, 0); x <= min(max(x1, x2) + pad, GFX_WIDTH - 1); ++x) {
            float px = x - x1, py = y - y1, t = (px * dx + py * dy) * mul;
            t = t < 0 ? 0 : t > 1 ? 1 : t;
            px -= t * dx;
            py -= t * dy;
            if (px * px + py * py <= r2) fb[y * GFX_WIDTH + x] = color;
        }
    }
}

void gfx_thick_arc(uint16_t *fb, int x, int y, int r, int a1, int a2, int sw, gfx_color_t color)
{
    if (r <= 0) return;
    const float rad = 0.01745329252f;
    int span = clamp(a2 - a1, -360, 360);
    if (span < 0) span += 360;
    if (!span && a1 != a2) span = 360;
    int steps = max(1, (span + 3) / 4);
    int px = x + (int)lroundf(cosf(a1 * rad) * r), py = y + (int)lroundf(sinf(a1 * rad) * r);
    for (int i = 1; i <= steps; ++i) {
        float angle = (a1 + (float)span * i / steps) * rad;
        int nx = x + (int)lroundf(cosf(angle) * r), ny = y + (int)lroundf(sinf(angle) * r);
        gfx_thick_line(fb, px, py, nx, ny, sw, color);
        px = nx;
        py = ny;
    }
}

static size_t line_len(const char *s, size_t columns)
{
    size_t n = 0, word_end = 0;
    while (n < columns && s[n] && s[n] != '\n') {
        if (s[n] == ' ') word_end = n;
        ++n;
    }
    return n == columns && s[n] && s[n] != ' ' && s[n] != '\n' && word_end ? word_end : n;
}

static const char *next_line(const char *s, size_t n)
{
    s += n;
    while (*s == ' ') ++s;
    return *s == '\n' ? s + 1 : s;
}

void gfx_text(uint16_t *fb, int x, int y, int w, int h, int fs, const char *text, gfx_color_t color)
{
    char ascii[257];
    size_t n = 0;
    for (const unsigned char *p = (const unsigned char *)text; *p && n < sizeof(ascii) - 1; ++p) {
        if ((*p & 0xc0) != 0x80) ascii[n++] = *p == '\n' || (*p >= 32 && *p <= 126) ? (char)*p : '?';
    }
    ascii[n] = '\0';
    fs = clamp(fs, 1, 3);
    int cell = 8 * fs, pitch = 10 * fs;
    int columns = w / cell, max_lines = h < cell ? 0 : 1 + (h - cell) / pitch;
    if (columns < 1 || max_lines < 1) return;
    int lines = 0;
    for (const char *p = ascii; *p && lines < max_lines; p = next_line(p, line_len(p, columns))) ++lines;
    int top = y + (h - (lines ? (lines - 1) * pitch + cell : 0)) / 2;
    const char *p = ascii;
    for (int line = 0; line < lines; ++line, top += pitch) {
        n = line_len(p, columns);
        int left = x + (w - (int)n * cell) / 2;
        for (size_t ch = 0; ch < n; ++ch, left += cell) {
            const uint8_t *glyph = font[(unsigned char)p[ch] - ' '];
            for (int row = 0; row < 8; ++row) {
                for (int col = 0; col < 8; ++col) {
                    if (glyph[row] & (1u << col)) slab(fb, left + col * fs, top + row * fs, fs, fs, color);
                }
            }
        }
        p = next_line(p, n);
    }
}
