#include "display.h"

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "bsp/display.h"
#include "bsp/touch.h"
#include "esp_check.h"
#include "esp_heap_caps.h"
#include "esp_lcd_panel_io.h"
#include "esp_lcd_panel_ops.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "media.h"
#include "scene.h"
#include "wifi.h"
#include "ws_client.h"

enum { STRIP_ROWS = 24, STRIP_BYTES = STRIP_ROWS * GFX_WIDTH * sizeof(uint16_t), BRIGHT = 85, DIM = 12 };
_Static_assert(BSP_LCD_H_RES == GFX_WIDTH && BSP_LCD_V_RES == GFX_HEIGHT && BSP_LCD_BIGENDIAN,
               "Framebuffer must match the ST77916");
static const char *TAG = "display";
static esp_lcd_panel_handle_t panel;
static esp_lcd_touch_handle_t touch;
static SemaphoreHandle_t drawn, scene_lock;
static TaskHandle_t painter;
static uint16_t *fb, *strip;
static scene_t *scene;
static bool linked;
static atomic_bool dirty;
static atomic_int brightness = BRIGHT;
static atomic_uint last_touch;

static uint32_t now_ms(void) { return (uint32_t)(esp_timer_get_time() / 1000); }

static bool color_done(esp_lcd_panel_io_handle_t io, esp_lcd_panel_io_event_data_t *event, void *ctx)
{
    BaseType_t awaken = pdFALSE;
    xSemaphoreGiveFromISR(ctx, &awaken);
    return awaken == pdTRUE;
}

static esp_err_t blit(void)
{
    for (int y = 0; y < GFX_HEIGHT; y += STRIP_ROWS) {
        int rows = GFX_HEIGHT - y < STRIP_ROWS ? GFX_HEIGHT - y : STRIP_ROWS;
        memcpy(strip, fb + y * GFX_WIDTH, rows * GFX_WIDTH * sizeof(*strip));
        ESP_RETURN_ON_ERROR(esp_lcd_panel_draw_bitmap(panel, 0, y, GFX_WIDTH, y + rows, strip), TAG, "Queue LCD strip");
        // tx_color borrows the strip until the SPI completion callback.
        xSemaphoreTake(drawn, portMAX_DELAY);
    }
    return ESP_OK;
}

static void render(void)
{
    if (linked && scene->ready) {
        scene_render(scene, fb);
    } else {
        char ip[WIFI_IP_LEN];
        wifi_get_ip(ip);
        bool wifi = strcmp(ip, "0.0.0.0") != 0;
        scene_render_offline(fb, wifi_character(), wifi ? ip : "",
                             linked ? "Waiting for scene" : wifi ? "Waiting for bridge" : "Joining Wi-Fi");
    }
}

static void paint_task(void *arg)
{
    int applied = BRIGHT;
    for (;;) {
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(500));
        xSemaphoreTake(scene_lock, portMAX_DELAY);
        if (!linked && now_ms() - atomic_load(&last_touch) >= 120000u) {
            atomic_store(&brightness, DIM);
        }
        bool redraw = atomic_exchange(&dirty, false);
        if (redraw) render();
        xSemaphoreGive(scene_lock);
        int level = atomic_load(&brightness);
        if (level != applied && bsp_display_brightness_set(level) == ESP_OK) applied = level;
        if (redraw) blit();
    }
}

void display_refresh(void)
{
    atomic_store(&dirty, true);
    if (painter) xTaskNotifyGive(painter);
}

void display_set_link(bool connected)
{
    xSemaphoreTake(scene_lock, portMAX_DELAY);
    linked = connected;
    scene->ready = false;
    xSemaphoreGive(scene_lock);
    display_refresh();
}

bool display_apply_scene(const cJSON *msg)
{
    xSemaphoreTake(scene_lock, portMAX_DELAY);
    bool applied = linked && scene_apply_json(scene, msg);
    xSemaphoreGive(scene_lock);
    if (applied) display_refresh();
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
    char id[SCENE_ID_CAP] = "";
    int x = 0, y = 0, sent_x = 0, sent_y = 0;
    uint32_t drag_at = 0;
    unsigned released = 0;
    bool down = false;
    TickType_t tick = xTaskGetTickCount();
    for (;;) {
        vTaskDelayUntil(&tick, pdMS_TO_TICKS(30));
        esp_lcd_touch_point_data_t p;
        uint8_t count = 0;
        if (esp_lcd_touch_read_data(touch) != ESP_OK ||
            esp_lcd_touch_get_data(touch, &p, &count, 1) != ESP_OK) continue;
        if (!count || p.x >= GFX_WIDTH || p.y >= GFX_HEIGHT) {
            if (down && ++released == 2) {
                down = false;
                ws_client_touch(WS_TOUCH_UP, id, x, y);
            }
            continue;
        }
        released = 0;
        x = p.x;
        y = p.y;
        if (!down) {
            down = true;
            display_wake();
            media_interrupt();
            xSemaphoreTake(scene_lock, portMAX_DELAY);
            const scene_node_t *hit = linked ? scene_hit_test(scene, x, y) : NULL;
            snprintf(id, sizeof(id), "%s", hit ? hit->id : "");
            xSemaphoreGive(scene_lock);
            ws_client_touch(WS_TOUCH_TAP, id, x, y);
        } else {
            if (now_ms() - drag_at < 60u || (abs(x - sent_x) < 4 && abs(y - sent_y) < 4)) continue;
            ws_client_touch(WS_TOUCH_DRAG, id, x, y);
        }
        drag_at = now_ms();
        sent_x = x;
        sent_y = y;
    }
}

esp_err_t display_init(void)
{
    fb = heap_caps_malloc(GFX_PIXELS * sizeof(*fb), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    strip = heap_caps_malloc(STRIP_BYTES, MALLOC_CAP_INTERNAL | MALLOC_CAP_DMA);
    scene = heap_caps_calloc(1, sizeof(*scene), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    drawn = xSemaphoreCreateBinary();
    scene_lock = xSemaphoreCreateMutex();
    ESP_RETURN_ON_FALSE(fb && strip && scene && drawn && scene_lock, ESP_ERR_NO_MEM, TAG, "Display buffers");
    bsp_display_config_t config = { .max_transfer_sz = STRIP_BYTES };
    esp_lcd_panel_io_handle_t io;
    ESP_RETURN_ON_ERROR(bsp_display_new(&config, &panel, &io), TAG, "Init LCD");
    const esp_lcd_panel_io_callbacks_t callbacks = { .on_color_trans_done = color_done };
    ESP_RETURN_ON_ERROR(esp_lcd_panel_io_register_event_callbacks(io, &callbacks, drawn), TAG, "LCD DMA callback");
    ESP_RETURN_ON_ERROR(esp_lcd_panel_disp_on_off(panel, true), TAG, "Enable LCD");
    ESP_RETURN_ON_ERROR(bsp_touch_new(NULL, &touch), TAG, "Init CST816S");
    render();
    ESP_RETURN_ON_ERROR(blit(), TAG, "First frame");
    ESP_RETURN_ON_ERROR(bsp_display_backlight_on(), TAG, "Enable backlight");
    ESP_RETURN_ON_ERROR(bsp_display_brightness_set(BRIGHT), TAG, "Set initial brightness");
    atomic_store(&last_touch, now_ms());
    ESP_RETURN_ON_FALSE(xTaskCreate(paint_task, "painter", 4096, NULL, 3, &painter) == pdPASS &&
                        xTaskCreate(touch_task, "touch", 3072, NULL, 4, NULL) == pdPASS,
                        ESP_ERR_NO_MEM, TAG, "Display tasks");
    return ESP_OK;
}
