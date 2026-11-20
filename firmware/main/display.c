#include "display.h"

#include <stdatomic.h>
#include <string.h>

#include "bsp/display.h"
#include "bsp/touch.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "frame.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "gfx.h"
#include "media.h"
#include "touch_gesture.h"
#include "wifi.h"
#include "ws_client.h"

enum { STRIP_ROWS = 24, STRIP_BYTES = STRIP_ROWS * FRAME_WIDTH * sizeof(uint16_t), BRIGHT = 85, DIM = 12 };
_Static_assert(BSP_LCD_H_RES == FRAME_WIDTH && BSP_LCD_V_RES == FRAME_HEIGHT && BSP_LCD_BIGENDIAN,
               "Framebuffer must match the LCD geometry and byte order");
static const char *TAG = "display";
static esp_lcd_panel_handle_t panel;
static esp_lcd_touch_handle_t touch;
static SemaphoreHandle_t drawn, fb_lock;
static TaskHandle_t painter;
static uint16_t *fb, *strip;
static bool linked;
static bool first_frame;
static int dirty_top, dirty_bottom;
static atomic_int brightness = BRIGHT;
static atomic_uint last_touch;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

static bool color_done(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *event, void *ctx)
{
    BaseType_t awaken = pdFALSE;
    xSemaphoreGiveFromISR(ctx, &awaken);
    return awaken == pdTRUE;
}

static esp_err_t blit(int top, int bottom)
{
    for (int y = top; y < bottom; y += STRIP_ROWS) {
        int rows = bottom - y < STRIP_ROWS ? bottom - y : STRIP_ROWS;
        xSemaphoreTake(fb_lock, portMAX_DELAY);
        memcpy(strip, fb + y * FRAME_WIDTH, rows * FRAME_WIDTH * sizeof(*strip));
        xSemaphoreGive(fb_lock);
        ESP_RETURN_ON_ERROR(esp_lcd_panel_draw_bitmap(panel, 0, y, FRAME_WIDTH, y + rows, strip), TAG, "Queue LCD strip");
        // tx_color borrows the strip until the SPI completion callback.
        xSemaphoreTake(drawn, portMAX_DELAY);
    }
    return ESP_OK;
}

static void mark_rows(int top, int bottom)
{
    if (dirty_top == dirty_bottom || top < dirty_top) dirty_top = top;
    if (bottom > dirty_bottom) dirty_bottom = bottom;
}

static void display_show_offline(void)
{
    gfx_offline_cat(fb, wifi_character());
}

static void paint_task(void *arg)
{
    int applied = BRIGHT;
    for (;;) {
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(500));
        xSemaphoreTake(fb_lock, portMAX_DELAY);
        if (!linked && now_ms() - atomic_load(&last_touch) >= 120000u) {
            atomic_store(&brightness, DIM);
        }
        int top = dirty_top, bottom = dirty_bottom;
        dirty_top = dirty_bottom = 0;
        xSemaphoreGive(fb_lock);
        int level = atomic_load(&brightness);
        if (level != applied && bsp_display_brightness_set(level) == ESP_OK) applied = level;
        if (top < bottom) blit(top, bottom);
    }
}

void display_set_link(bool connected)
{
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    linked = connected;
    first_frame = connected;
    if (!connected) {
        display_show_offline();
        mark_rows(0, FRAME_HEIGHT);
    }
    xSemaphoreGive(fb_lock);
    if (!connected) xTaskNotifyGive(painter);
}

bool display_apply_frame(const uint8_t *data, size_t len)
{
    frame_rect_t dirty;
    xSemaphoreTake(fb_lock, portMAX_DELAY);
    bool applied = linked && frame_apply(fb, data, len, &dirty);
    if (applied) mark_rows(dirty.y, dirty.y + dirty.h);
    bool first = applied && first_frame;
    if (first) first_frame = false;
    xSemaphoreGive(fb_lock);
    if (first) ESP_LOGI(TAG, "Applied bridge frame 0x03 %ux%u (codec %u)", dirty.w, dirty.h, data[9]);
    if (applied) xTaskNotifyGive(painter);
    return applied;
}

void display_set_brightness(int level)
{
    if (level < DIM || level > BRIGHT) return;
    atomic_store(&brightness, level);
    if (painter) xTaskNotifyGive(painter);
}

void display_wake(void)
{
    atomic_store(&last_touch, now_ms());
    if (atomic_load(&brightness) != BRIGHT) display_set_brightness(BRIGHT);
}

bool display_is_sleeping(void)
{
    return atomic_load(&brightness) == DIM;
}

static void touch_task(void *arg)
{
    touch_gesture_t gesture = { 0 };
    TickType_t tick = xTaskGetTickCount();
    for (;;) {
        vTaskDelayUntil(&tick, pdMS_TO_TICKS(30));
        esp_lcd_touch_point_data_t p = { 0 };
        uint8_t count = 0;
        if (esp_lcd_touch_read_data(touch) != ESP_OK ||
            esp_lcd_touch_get_data(touch, &p, &count, 1) != ESP_OK) continue;
        bool pressed = count && p.x < FRAME_WIDTH && p.y < FRAME_HEIGHT;
        touch_event_t event = touch_gesture_update(&gesture, pressed, p.x, p.y, now_ms());
        switch (event.kind) {
        case TOUCH_PRESS:
            display_wake();
            media_interrupt();
            break;
        case TOUCH_TAP: ws_client_pointer("tap", event.x, event.y); break;
        case TOUCH_DRAG: ws_client_pointer("drag", event.x, event.y); break;
        case TOUCH_UP: ws_client_pointer("up", event.x, event.y); break;
        case TOUCH_NONE: break;
        }
    }
}

esp_err_t display_init(void)
{
    fb = heap_caps_malloc(FRAME_PIXELS * sizeof(*fb), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    strip = heap_caps_malloc(STRIP_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_DMA);
    drawn = xSemaphoreCreateBinary();
    fb_lock = xSemaphoreCreateMutex();
    ESP_RETURN_ON_FALSE(fb && strip && drawn && fb_lock, ESP_ERR_NO_MEM, TAG, "Display buffers");
    bsp_display_config_t config = { .max_transfer_sz = STRIP_BYTES };
    esp_lcd_panel_io_handle_t io;
    ESP_RETURN_ON_ERROR(bsp_display_new(&config, &panel, &io), TAG, "Init LCD");
    const esp_lcd_panel_io_callbacks_t callbacks = { .on_color_trans_done = color_done };
    ESP_RETURN_ON_ERROR(esp_lcd_panel_io_register_event_callbacks(io, &callbacks, drawn), TAG, "LCD DMA callback");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(panel, true), TAG, "Enable LCD");
    // BOX-3's BSP sets the panel to landscape and already aligns GT911 coordinates.
    ESP_RETURN_ON_ERROR(bsp_touch_new(NULL, &touch), TAG, "Init " BOARD_TOUCH);
    display_show_offline();
    ESP_RETURN_ON_ERROR(blit(0, FRAME_HEIGHT), TAG, "First frame");
    ESP_RETURN_ON_ERROR(bsp_display_backlight_on(), TAG, "Enable backlight");
    ESP_RETURN_ON_ERROR(bsp_display_brightness_set(BRIGHT), TAG, "Set initial brightness");
    atomic_store(&last_touch, now_ms());
    ESP_RETURN_ON_FALSE(xTaskCreate(paint_task, "painter", 4096, NULL, 3, &painter) == pdPASS &&
                        xTaskCreate(touch_task, "touch", 3072, NULL, 4, NULL) == pdPASS,
                        ESP_ERR_NO_MEM, TAG, "Display tasks");
    ESP_LOGI(TAG, BOARD_LCD " %dx%d LCD and " BOARD_TOUCH " touch initialized", FRAME_WIDTH, FRAME_HEIGHT);
    return ESP_OK;
}
