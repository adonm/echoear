#include "touch_gesture.h"

#include <stdlib.h>

enum { RELEASE_POLLS = 2, DRAG_DISTANCE = 10, DRAG_INTERVAL_MS = 60 };

touch_event_t touch_gesture_update(touch_gesture_t *g, bool pressed, uint16_t x, uint16_t y, uint32_t now_ms)
{
    if (!pressed) {
        // Ignore a single missing poll, as either controller can miss a sample.
        if (!g->down || ++g->released < RELEASE_POLLS) return (touch_event_t){ 0 };
        g->down = false;
        if (g->dragging) return (touch_event_t){ TOUCH_UP, g->last_x, g->last_y };
        return (touch_event_t){ TOUCH_TAP, g->start_x, g->start_y };
    }
    if (!g->down) {
        *g = (touch_gesture_t){
            .down = true, .start_x = x, .start_y = y, .last_x = x, .last_y = y, .sent_x = x, .sent_y = y,
        };
        return (touch_event_t){ TOUCH_PRESS, x, y };
    }

    g->released = 0;
    g->last_x = x;
    g->last_y = y;
    if (abs((int)x - g->start_x) + abs((int)y - g->start_y) >= DRAG_DISTANCE) g->dragging = true;
    if (!g->dragging || now_ms - g->last_drag_ms < DRAG_INTERVAL_MS ||
        (x == g->sent_x && y == g->sent_y)) return (touch_event_t){ 0 };
    g->last_drag_ms = now_ms;
    g->sent_x = x;
    g->sent_y = y;
    return (touch_event_t){ TOUCH_DRAG, x, y };
}
