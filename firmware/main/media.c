#include "media.h"

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "display.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"
#include "ws_client.h"

enum { TTS_BYTES = 1024 * 1024, TTS_BLOCK = 1280 };
typedef enum { RECORD, STREAM, SPEAK, CHIME } job_kind_t;
typedef struct {
    job_kind_t kind;
    uint32_t session;
    char target[MEDIA_TARGET_CAP];
    int16_t *pcm;
    size_t samples;
    audio_tone_t tone;
} job_t;

static const char *TAG = "media";
static QueueHandle_t jobs;
static SemaphoreHandle_t lock;
static StreamBufferHandle_t tts;
static StaticStreamBuffer_t tts_control;
static bool recording, http_speech;
static atomic_bool stream_open, stream_ended, speech_busy, stop_recording, cancel_recording, interrupted;

static bool submit(const job_t *job)
{
    return xQueueSend(jobs, job, 0) == pdPASS;
}

void media_interrupt(void)
{
    if (atomic_load(&speech_busy)) {
        atomic_store(&interrupted, true);
        audio_stop_playback();
    }
}

void media_link_lost(void)
{
    atomic_store(&cancel_recording, true);
    if (atomic_load(&stream_open)) media_interrupt();
}

bool media_record_start(const char *target)
{
    if (!jobs || !target || strlen(target) >= MEDIA_TARGET_CAP) return false;
    job_t job = { .kind = RECORD, .session = ws_client_session() };
    snprintf(job.target, sizeof(job.target), "%s", target);
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = !recording && !http_speech && submit(&job);
    if (accepted) {
        recording = true;
        atomic_store(&cancel_recording, false);
        atomic_store(&stop_recording, false);
        media_interrupt();
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_record_stop(void)
{
    atomic_store(&stop_recording, true);
}

static void prepare_playback(void)
{
    atomic_store(&interrupted, false);
    atomic_store(&speech_busy, true);
}

bool media_tts_start(void)
{
    if (!jobs) return false;
    job_t job = { .kind = STREAM, .session = ws_client_session() };
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = !recording && !http_speech && !atomic_load(&stream_open) &&
                    xStreamBufferReset(tts) == pdPASS && submit(&job);
    if (accepted) {
        prepare_playback();
        atomic_store(&stream_ended, false);
        atomic_store(&stream_open, true);
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_tts_end(void)
{
    atomic_store(&stream_ended, true);
}

bool media_tts_pcm(const void *data, size_t bytes)
{
    const TickType_t start = xTaskGetTickCount();
    const TickType_t timeout = pdMS_TO_TICKS(15000), slice = pdMS_TO_TICKS(100);
    const uint8_t *pcm = data;
    while (bytes) {
        if (!atomic_load(&stream_open) || atomic_load(&stream_ended) || atomic_load(&interrupted)) return false;
        TickType_t elapsed = xTaskGetTickCount() - start;
        if (elapsed >= timeout) {
            ESP_LOGW(TAG, "Speaker stream buffer overrun");
            media_interrupt();
            return false;
        }
        TickType_t remaining = timeout - elapsed;
        size_t sent = xStreamBufferSend(tts, pcm, bytes, remaining < slice ? remaining : slice);
        pcm += sent;
        bytes -= sent;
    }
    return true;
}

void media_chime(audio_tone_t tone)
{
    if (!jobs) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (!recording) {
        if (atomic_load(&speech_busy) && tone == AUDIO_HAPPY_TONE) {
            audio_request_chime();
        } else {
            submit(&(job_t){ .kind = CHIME, .tone = tone });
        }
    }
    xSemaphoreGive(lock);
}

esp_err_t media_submit_speech(int16_t *pcm, size_t samples)
{
    if (!jobs) return ESP_ERR_NOT_SUPPORTED;
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = !recording && !atomic_load(&speech_busy) &&
                    submit(&(job_t){ .kind = SPEAK, .pcm = pcm, .samples = samples });
    if (accepted) {
        http_speech = true;
        prepare_playback();
    }
    xSemaphoreGive(lock);
    return accepted ? ESP_OK : ESP_ERR_INVALID_STATE;
}

static bool stop_capture(void)
{
    return atomic_load(&stop_recording) || atomic_load(&cancel_recording);
}

static esp_err_t upload_pcm(const int16_t *pcm, size_t samples, void *ctx)
{
    const job_t *job = ctx;
    return !atomic_load(&cancel_recording) && ws_client_rec_pcm(job->session, pcm, samples) ? ESP_OK : ESP_FAIL;
}

static void record(job_t *job)
{
    bool started = !atomic_load(&cancel_recording) && ws_client_rec_start(job->session, job->target);
    bool cancelled = true;
    if (started) {
        audio_tone(AUDIO_START_TONE);
        esp_err_t err = audio_record_stream(stop_capture, upload_pcm, job);
        cancelled = err != ESP_OK || atomic_load(&cancel_recording);
    }
    xSemaphoreTake(lock, portMAX_DELAY);
    recording = false;
    xSemaphoreGive(lock);
    if (started) ws_client_rec_end(job->session, job->target, cancelled);
}

static void play(job_t *job)
{
    display_wake();
    audio_stream_begin();
    if (atomic_load(&interrupted)) audio_stop_playback();
    esp_err_t err = ESP_OK;
    if (job->kind == STREAM) {
        int16_t chunk[TTS_BLOCK / sizeof(int16_t)];
        while (!atomic_load(&interrupted) && err == ESP_OK) {
            size_t bytes = xStreamBufferReceive(tts, chunk, sizeof(chunk), pdMS_TO_TICKS(30));
            if (bytes % sizeof(*chunk)) {
                err = ESP_ERR_INVALID_SIZE;
            } else if (bytes) {
                err = audio_stream_write(chunk, bytes / sizeof(*chunk));
            } else if (atomic_load(&stream_ended) && !xStreamBufferBytesAvailable(tts)) {
                break;
            }
        }
    } else {
        err = audio_stream_write(job->pcm, job->samples);
        free(job->pcm);
    }
    esp_err_t end = audio_stream_end();
    xSemaphoreTake(lock, portMAX_DELAY);
    bool aborted = atomic_load(&interrupted) || err != ESP_OK || end != ESP_OK;
    atomic_store(&speech_busy, false);
    if (job->kind == STREAM) {
        atomic_store(&stream_open, false);
    } else {
        http_speech = false;
    }
    xSemaphoreGive(lock);
    if (job->kind == STREAM) ws_client_tts_ack(job->session, aborted);
}

static void worker(void *arg)
{
    for (;;) {
        job_t job;
        xQueueReceive(jobs, &job, portMAX_DELAY);
        // The producer publishes cancellation and ownership before the worker uses the job.
        xSemaphoreTake(lock, portMAX_DELAY);
        xSemaphoreGive(lock);
        if (job.kind == RECORD) {
            record(&job);
        } else if (job.kind == CHIME) {
            audio_tone(job.tone);
        } else {
            play(&job);
        }
    }
}

esp_err_t media_init(bool audio_ready)
{
    if (!audio_ready) return ESP_OK;
    uint8_t *storage = heap_caps_malloc(TTS_BYTES + 1, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    if (!storage) return ESP_ERR_NO_MEM;
    tts = xStreamBufferCreateStatic(TTS_BYTES + 1, sizeof(int16_t), storage, &tts_control);
    lock = xSemaphoreCreateMutex();
    jobs = xQueueCreate(6, sizeof(job_t));
    return tts && lock && jobs && xTaskCreate(worker, "media", 7168, NULL, 5, NULL) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}
