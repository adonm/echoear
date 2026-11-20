#include "media.h"

#include <stdatomic.h>
#include <stdio.h>
#include <string.h>

#include "display.h"
#include "esp_heap_caps.h"
#include "freertos/FreeRTOS.h"
#include "freertos/queue.h"
#include "freertos/semphr.h"
#include "freertos/stream_buffer.h"
#include "freertos/task.h"
#include "telemetry.h"
#include "ws_client.h"

enum { TTS_BYTES = 1024 * 1024, TTS_BLOCK = 1280 };
typedef enum { RECORD, STREAM, CHIME } job_kind_t;
typedef struct {
    job_kind_t kind;
    uint32_t session;
    char target[MEDIA_TARGET_CAP];
    audio_tone_t tone;
} job_t;

static QueueHandle_t jobs;
static SemaphoreHandle_t lock;
static TaskHandle_t media_task;
static StreamBufferHandle_t tts;
static StaticStreamBuffer_t tts_control;
// A queued STREAM job is a wakeup for the latest generation, not a second buffer owner.
static struct {
    uint32_t serial, session, audio_id;
    bool open, ended, queued;
} stream;
static atomic_bool recording, stop_recording, cancel_recording;

static bool submit(const job_t *job)
{
    // Leave room for a replacement stream even if chimes are queued.
    if (job->kind != STREAM && uxQueueSpacesAvailable(jobs) <= 1) return false;
    return xQueueSend(jobs, job, 0) == pdPASS;
}

static void interrupt_locked(void)
{
    if (stream.open) {
        audio_stop_playback();
        stream.open = false;
        xTaskNotifyGive(media_task);
        ws_client_tts_ack(stream.session, stream.audio_id, true);
    }
}

void media_interrupt(void)
{
    if (!lock) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    interrupt_locked();
    xSemaphoreGive(lock);
}

void media_link_lost(void)
{
    atomic_store(&cancel_recording, true);
    media_interrupt();
}

bool media_record_start(const char *target)
{
    if (!jobs || !target || strlen(target) >= MEDIA_TARGET_CAP) return false;
    job_t job = { .kind = RECORD, .session = ws_client_session() };
    snprintf(job.target, sizeof(job.target), "%s", target);
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = !recording && submit(&job);
    if (accepted) {
        recording = true;
        atomic_store(&cancel_recording, false);
        atomic_store(&stop_recording, false);
        interrupt_locked();
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_record_stop(void)
{
    atomic_store(&stop_recording, true);
}

bool media_is_recording(void)
{
    return atomic_load(&recording);
}

bool media_tts_start(uint32_t audio_id)
{
    if (!jobs) return false;
    xSemaphoreTake(lock, portMAX_DELAY);
    uint32_t session = ws_client_session();
    bool accepted = session && !recording && !stream.open && xStreamBufferReset(tts) == pdPASS &&
                    (stream.queued || submit(&(job_t){ .kind = STREAM }));
    if (accepted) {
        ++stream.serial;
        stream.session = session;
        stream.audio_id = audio_id;
        stream.open = stream.queued = true;
        stream.ended = false;
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_tts_end(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    if (stream.open) {
        stream.ended = true;
        xTaskNotifyGive(media_task);
    }
    xSemaphoreGive(lock);
}

bool media_tts_pcm(const void *data, size_t bytes)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    bool accepted = false;
    if (stream.open && !stream.ended) {
        // The WS callback must not wait for speaker space; writes and reads
        // only hold this lock for the short buffer copies, never codec I/O.
        if (xStreamBufferSpacesAvailable(tts) >= bytes && xStreamBufferSend(tts, data, bytes, 0) == bytes) {
            accepted = true;
            xTaskNotifyGive(media_task);
        } else {
            atomic_fetch_add_explicit(&telemetry_io.audio_drops, 1, memory_order_relaxed);
            interrupt_locked();
        }
    }
    xSemaphoreGive(lock);
    return accepted;
}

void media_chime(audio_tone_t tone)
{
    if (!jobs) return;
    xSemaphoreTake(lock, portMAX_DELAY);
    if (!recording) {
        if (stream.open && tone == AUDIO_HAPPY_TONE) {
            audio_request_chime();
        } else {
            submit(&(job_t){ .kind = CHIME, .tone = tone });
        }
    }
    xSemaphoreGive(lock);
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

static bool owns_stream(uint32_t serial)
{
    return stream.open && stream.serial == serial;
}

static void play(void)
{
    xSemaphoreTake(lock, portMAX_DELAY);
    stream.queued = false;
    bool open = stream.open;
    uint32_t serial = stream.serial;
    if (open) audio_stream_begin();
    xSemaphoreGive(lock);
    if (!open) return;

    display_wake();
    int16_t chunk[TTS_BLOCK / sizeof(int16_t)];
    esp_err_t err = ESP_OK;
    bool played = false, starved = false;
    while (err == ESP_OK) {
        xSemaphoreTake(lock, portMAX_DELAY);
        bool owns = owns_stream(serial);
        // Keep receive/reset serialized; waits and codec I/O must not hold this lock.
        size_t bytes = owns ? xStreamBufferReceive(tts, chunk, sizeof(chunk), 0) : 0;
        bool done = !owns || (stream.ended && !bytes);
        xSemaphoreGive(lock);
        if (done) break;
        if (bytes % sizeof(*chunk)) {
            err = ESP_ERR_INVALID_SIZE;
        } else if (bytes) {
            err = audio_stream_write(chunk, bytes / sizeof(*chunk));
            played = true;
            starved = false;
        } else {
            // Count starvation once per gap after playback has started, not
            // the initial prefill or the normal drain after an end frame.
            if (played && !starved) {
                atomic_fetch_add_explicit(&telemetry_io.audio_underruns, 1, memory_order_relaxed);
                starved = true;
            }
            ulTaskNotifyTake(pdTRUE, portMAX_DELAY);
        }
    }
    esp_err_t end = audio_stream_end();
    xSemaphoreTake(lock, portMAX_DELAY);
    if (owns_stream(serial)) {
        stream.open = false;
        ws_client_tts_ack(stream.session, stream.audio_id, err != ESP_OK || end != ESP_OK);
    }
    xSemaphoreGive(lock);
}

static void worker(void *arg)
{
    (void)arg;
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
            play();
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
    return tts && lock && jobs && xTaskCreate(worker, "media", 7168, NULL, 5, &media_task) == pdPASS
           ? ESP_OK : ESP_ERR_NO_MEM;
}
