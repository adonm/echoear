#pragma once

#include <stdbool.h>
#include <stdint.h>

typedef enum { TOUCH_NONE, TOUCH_PRESS, TOUCH_TAP, TOUCH_DRAG, TOUCH_UP } touch_event_kind_t;
typedef struct {
    touch_event_kind_t kind;
    uint16_t x, y;
} touch_event_t;

typedef struct {
    bool down, dragging;
    unsigned released;
    uint16_t start_x, start_y, last_x, last_y, sent_x, sent_y;
    uint32_t last_drag_ms;
} touch_gesture_t;

touch_event_t touch_gesture_update(touch_gesture_t *gesture, bool pressed, uint16_t x, uint16_t y, uint32_t now_ms);
