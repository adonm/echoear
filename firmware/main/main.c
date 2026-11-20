#include "audio.h"
#include "bridge_client.h"
#include "conversation.h"
#include "head_touch.h"
#include "ota_server.h"
#include "power.h"
#include "ui.h"
#include "wifi.h"

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "nvs_flash.h"

static QueueHandle_t actions;
static TaskHandle_t worker;
static bool audio_ready;

typedef enum { WORK_IDLE, WORK_BUSY, WORK_SPEAKING, WORK_FOLLOWUP } work_mode_t;
static _Atomic work_mode_t mode;

typedef enum { JOB_RECORD, JOB_RESET, JOB_SPEAK, JOB_ASK, JOB_CHIME } job_kind_t;
typedef struct {
    job_kind_t kind;
    void *data;
    size_t samples;
} job_t;

static esp_err_t submit(job_t job, bool followup)
{
    work_mode_t prior = WORK_IDLE;
    if (!atomic_compare_exchange_strong(&mode, &prior, WORK_BUSY)) {
        if (!followup || prior != WORK_SPEAKING ||
            !atomic_compare_exchange_strong(&mode, &prior, WORK_FOLLOWUP)) {
            return ESP_ERR_INVALID_STATE;
        }
        audio_stop_playback();
    }
    ESP_ERROR_CHECK(xQueueSend(actions, &job, 0) == pdPASS ? ESP_OK : ESP_ERR_INVALID_STATE);
    return ESP_OK;
}

esp_err_t conversation_submit_speech(int16_t *pcm, size_t samples)
{
    if (!audio_ready) {
        return ESP_ERR_NOT_SUPPORTED;
    }
    return submit((job_t){ JOB_SPEAK, pcm, samples }, false);
}

esp_err_t conversation_ask(const char *question)
{
    size_t len = strnlen(question, CONVERSATION_QUESTION_MAX + 1);
    if (!len || len > CONVERSATION_QUESTION_MAX) {
        return ESP_ERR_INVALID_ARG;
    }
    char *copy = strdup(question);
    if (!copy) {
        return ESP_ERR_NO_MEM;
    }
    esp_err_t err = submit((job_t){ JOB_ASK, copy, 0 }, true);
    if (err != ESP_OK) {
        free(copy);
    }
    return err;
}

esp_err_t conversation_start_intercom(const char *target, const char *character)
{
    if (!audio_ready) {
        return ESP_ERR_NOT_SUPPORTED;
    }
    if (!bridge_client_valid_target(target) || !bridge_client_valid_target(character)) {
        return ESP_ERR_INVALID_ARG;
    }
    bridge_peer_t *copy = malloc(sizeof(*copy));
    if (!copy) {
        return ESP_ERR_NO_MEM;
    }
    strcpy(copy->id, target);
    strcpy(copy->character, character);
    esp_err_t err = submit((job_t){ .kind = JOB_RECORD, .data = copy }, true);
    if (err != ESP_OK) {
        free(copy);
    }
    return err;
}

void conversation_chime(void)
{
    if (!audio_ready) {
        return;
    }
    if (atomic_load(&mode) == WORK_SPEAKING) {
        audio_request_chime();
    } else {
        submit((job_t){ .kind = JOB_CHIME }, false);
    }
}

esp_err_t conversation_stream_begin(const char *caption)
{
    if (!audio_ready) {
        return ESP_ERR_NOT_SUPPORTED;
    }
    audio_stream_begin();
    atomic_store(&mode, WORK_SPEAKING);
    ui_set_phase(UI_SPEAKING, caption);
    return ESP_OK;
}

esp_err_t conversation_stream_end(void)
{
    esp_err_t err = audio_stream_end();
    work_mode_t speaking = WORK_SPEAKING;
    atomic_compare_exchange_strong(&mode, &speaking, WORK_BUSY);
    return err;
}

esp_err_t conversation_play(const int16_t *pcm, size_t samples)
{
    esp_err_t err = conversation_stream_begin(NULL);
    if (err != ESP_OK) {
        return err;
    }
    err = audio_stream_write(pcm, samples);
    bool cancelled = audio_playback_stopped();
    esp_err_t end = conversation_stream_end();
    return err == ESP_OK || cancelled ? end : err;
}

static bool handle_action(ui_action_t action)
{
    if (action == UI_STOP_RECORDING) {
        xTaskNotifyGive(worker);
        return true;
    }
    if (action == UI_STOP_SPEAKING) {
        audio_stop_playback();
        return true;
    }
    job_kind_t kind = action == UI_NEW_CHAT ? JOB_RESET : JOB_RECORD;
    return submit((job_t){ .kind = kind }, false) == ESP_OK;
}

static bool stop_requested(void)
{
    return ulTaskNotifyTake(pdTRUE, 0) > 0;
}

static void on_wifi(bool connected, const char *ip)
{
    ui_set_network(connected, ip);
    if (connected) {
        ota_confirm_boot();
    }
}

static const char *record_and_send(const bridge_peer_t *target, char error[128])
{
    audio_tone(target ? AUDIO_HAPPY_TONE : AUDIO_START_TONE);
    const int16_t *pcm;
    size_t bytes;
    esp_err_t err = audio_record(stop_requested, &pcm, &bytes);
    ui_set_phase(UI_BUSY, target ? "Sending..." : NULL);
    // A stop tap can race the recording limit. Don't carry it into the next recording.
    stop_requested();
    if (err == ESP_ERR_NOT_FOUND) {
        audio_tone(AUDIO_QUIET_TONE);
        return "Didn't hear you";
    }
    if (err != ESP_OK) {
        return "Mic error";
    }
    if (!bytes) {
        audio_tone(AUDIO_QUIET_TONE);
        return "Ready to try again!";
    }
    audio_tone(AUDIO_THINK_TONE);
    if (target) {
        return bridge_client_send_intercom(target->id, pcm, bytes / sizeof(*pcm)) == ESP_OK ? NULL : "Couldn't send";
    }
    return bridge_voice(pcm, bytes, error, 128) == ESP_OK ? NULL : error;
}

static bool mark_idle(void)
{
    work_mode_t busy = WORK_BUSY;
    return atomic_compare_exchange_strong(&mode, &busy, WORK_IDLE);
}

static void conversation_task(void *arg)
{
    for (;;) {
        job_t job;
        xQueueReceive(actions, &job, portMAX_DELAY);
        atomic_store(&mode, WORK_BUSY);
        char error[128];
        const char *caption = NULL;
        switch (job.kind) {
        case JOB_RECORD:
            caption = record_and_send(job.data, error);
            break;
        case JOB_RESET:
            if (bridge_reset(error, sizeof(error)) == ESP_OK) {
                ui_clear_reply();
                if (audio_ready) {
                    audio_tone(AUDIO_HAPPY_TONE);
                }
                caption = "Fresh chat!";
            } else {
                caption = "Couldn't reset";
            }
            break;
        case JOB_SPEAK:
            if (conversation_play(job.data, job.samples) != ESP_OK) {
                caption = "Speaker error";
            }
            break;
        case JOB_ASK:
            ui_set_phase(UI_BUSY, "Asking...");
            if (audio_ready) {
                audio_tone(AUDIO_HAPPY_TONE);
            }
            if (bridge_client_ask_text(job.data) != ESP_OK) {
                caption = "Couldn't ask";
            }
            break;
        case JOB_CHIME:
            audio_tone(AUDIO_HAPPY_TONE);
            break;
        }
        const bridge_peer_t *target = job.kind == JOB_RECORD ? job.data : NULL;
        ui_finish_turn(mark_idle, caption, target && !caption ? target->character : NULL);
        free(job.data);
    }
}

void app_main(void)
{
    esp_err_t nvs = nvs_flash_init();
    if (nvs == ESP_ERR_NVS_NO_FREE_PAGES || nvs == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        nvs = nvs_flash_init();
    }
    ESP_ERROR_CHECK(nvs);
    ESP_ERROR_CHECK(wifi_init_identity());
    actions = xQueueCreate(1, sizeof(job_t));
    ESP_ERROR_CHECK(actions ? ESP_OK : ESP_ERR_NO_MEM);
    ESP_ERROR_CHECK(ui_init(device_id + sizeof("echoear-") - 1, device_mac_bytes, handle_action));
    audio_ready = audio_init() == ESP_OK;
    ui_set_audio_ready(audio_ready);
    ESP_ERROR_CHECK(xTaskCreate(conversation_task, "conversation", 8192, NULL, 5, &worker) == pdPASS
                    ? ESP_OK : ESP_ERR_NO_MEM);
    if (head_touch_start() != ESP_OK) {
        ESP_LOGW("echoear", "Head touch task unavailable");
    }
    if (power_start() != ESP_OK) {
        ESP_LOGW("echoear", "Battery monitor unavailable");
    }
    if (bridge_client_start_registration() != ESP_OK) {
        ESP_LOGW("echoear", "Bridge registration task unavailable");
    }
    ESP_ERROR_CHECK(wifi_start(on_wifi));
    ESP_ERROR_CHECK(ota_server_start());
    ESP_LOGI("echoear", "%s ready", device_id);
}
