#include <assert.h>
#include <stdio.h>

#include "touch_gesture.h"

static void expect(touch_event_t event, touch_event_kind_t kind, uint16_t x, uint16_t y)
{
    assert(event.kind == kind);
    if (kind != TOUCH_NONE) assert(event.x == x && event.y == y);
}

int main(void)
{
    touch_gesture_t g = { 0 };
    expect(touch_gesture_update(&g, false, 0, 0, 970), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 50, 60, 1000), TOUCH_PRESS, 50, 60);
    expect(touch_gesture_update(&g, false, 0, 0, 1030), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1060), TOUCH_TAP, 50, 60);
    expect(touch_gesture_update(&g, false, 0, 0, 1090), TOUCH_NONE, 0, 0);

    expect(touch_gesture_update(&g, true, 50, 60, 1200), TOUCH_PRESS, 50, 60);
    expect(touch_gesture_update(&g, true, 54, 55, 1230), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1260), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1290), TOUCH_TAP, 50, 60);

    expect(touch_gesture_update(&g, true, 50, 60, 1500), TOUCH_PRESS, 50, 60);
    expect(touch_gesture_update(&g, true, 55, 55, 1530), TOUCH_DRAG, 55, 55);
    expect(touch_gesture_update(&g, false, 0, 0, 1560), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 58, 54, 1589), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 58, 54, 1590), TOUCH_DRAG, 58, 54);
    expect(touch_gesture_update(&g, true, 58, 54, 1650), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 50, 60, 1680), TOUCH_DRAG, 50, 60);
    expect(touch_gesture_update(&g, true, 53, 60, 1710), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1740), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1770), TOUCH_UP, 53, 60);
    expect(touch_gesture_update(&g, false, 0, 0, 1800), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 70, 80, 1830), TOUCH_PRESS, 70, 80);
    expect(touch_gesture_update(&g, false, 0, 0, 1860), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, false, 0, 0, 1890), TOUCH_TAP, 70, 80);

    expect(touch_gesture_update(&g, true, 50, 60, UINT32_MAX - 59), TOUCH_PRESS, 50, 60);
    expect(touch_gesture_update(&g, true, 60, 60, UINT32_MAX - 29), TOUCH_DRAG, 60, 60);
    expect(touch_gesture_update(&g, true, 70, 60, 29), TOUCH_NONE, 0, 0);
    expect(touch_gesture_update(&g, true, 70, 60, 30), TOUCH_DRAG, 70, 60);
    puts("PASS touch: tap origin, 10 px drag threshold, 60 ms throttle, release debounce, last point on up");
}
