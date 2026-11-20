#include "ws_client.h"

#include <inttypes.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cJSON.h"
#include "display.h"
#include "esp_app_desc.h"
#include "esp_crt_bundle.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_timer.h"
#include "esp_transport_ssl.h"
#include "esp_transport_tcp.h"
#include "esp_transport_ws.h"
#include "esp_websocket_client.h"
#include "frame.h"
#include "freertos/queue.h"
#include "freertos/task.h"
#include "lwip/sockets.h"
#include "media.h"
#include "power.h"
#include "sdkconfig.h"
#include "telemetry.h"
#include "wifi.h"

enum { TEXT_CAP = 32 * 1024, RECORD_PCM = 1, SPEAKER_PCM = 2, STATUS_MS = 5000 };
_Static_assert(FRAME_PACKET_CAP >= FRAME_HEADER_SIZE + FRAME_PIXELS * sizeof(uint16_t) &&
               (size_t)FRAME_PACKET_CAP >= TEXT_CAP,
               "Receive buffer must fit full raw frames and text");
typedef enum { INPUT_TAP, INPUT_DRAG, INPUT_UP, INPUT_HEAD, TTS_ACK, REJECT_RECORD } input_kind_t;
typedef struct {
    input_kind_t kind;
    uint32_t session;
    uint32_t audio_id;
    bool aborted;
    int x, y;
    char target[MEDIA_TARGET_CAP];
} input_t;
typedef struct {
    uint8_t opcode, type, carry;
    size_t used;
    bool drop, odd;
} fragment_t;

static const char *TAG = "ws";
static esp_websocket_client_handle_t client;
static esp_transport_handle_t ws_transport;
static TaskHandle_t service;
static QueueHandle_t inputs;
static uint8_t *rx_buffer;
static fragment_t rx;
static bool tts_accepted;
static atomic_bool wifi_online, finished;
static atomic_uint socket_session, ready_session;
static atomic_int socket_fd = -1;
static uint32_t next_session;
static int reconnect_ms = 1000;

uint32_t ws_client_session(void)
{
    return atomic_load(&socket_session);
}

static uint32_t ready_socket_session(void)
{
    uint32_t session = ws_client_session();
    return atomic_load(&wifi_online) && atomic_load(&ready_session) == session ? session : 0;
}

bool ws_client_connected(void)
{
    return ready_socket_session() != 0;
}

static bool send_wire(uint32_t session, const void *data, int len, bool binary, bool hello, TickType_t timeout)
{
    if (!atomic_load(&wifi_online) || !session || session != ws_client_session() ||
        (!hello && session != atomic_load(&ready_session))) return false;
    int written = binary ? esp_websocket_client_send_bin(client, data, len, timeout)
                         : esp_websocket_client_send_text(client, data, len, timeout);
    return written == len;
}

static bool send_ready(uint32_t session, const void *data, int len, bool binary)
{
    return send_wire(session, data, len, binary, false, pdMS_TO_TICKS(500));
}

static cJSON *event_json(const char *event)
{
    cJSON *json = cJSON_CreateObject();
    if (!cJSON_AddStringToObject(json, "ev", event)) {
        cJSON_Delete(json);
        return NULL;
    }
    return json;
}

static bool send_json(uint32_t session, cJSON *json)
{
    char buffer[512];
    bool sent = json && cJSON_PrintPreallocated(json, buffer, sizeof(buffer), false) &&
                send_ready(session, buffer, strlen(buffer), false);
    cJSON_Delete(json);
    return sent;
}

static void enqueue_session(input_t input)
{
    if (input.session && inputs && xQueueSend(inputs, &input, 0) == pdPASS) xTaskNotifyGive(service);
}

static void enqueue(input_t input)
{
    input.session = ws_client_session();
    enqueue_session(input);
}

void ws_client_pointer(const char *event, int x, int y)
{
    uint32_t session = ready_socket_session();
    if (!session) return;
    input_kind_t kind;
    if (!strcmp(event, "tap")) kind = INPUT_TAP;
    else if (!strcmp(event, "drag")) kind = INPUT_DRAG;
    else if (!strcmp(event, "up")) kind = INPUT_UP;
    else return;
    enqueue_session((input_t){ .kind = kind, .session = session, .x = x, .y = y });
}

void ws_client_head_pet(int volume)
{
    enqueue((input_t){ .kind = INPUT_HEAD, .x = volume });
}

bool ws_client_rec_start(uint32_t session, const char *target)
{
    cJSON *json = event_json("rec_start");
    if (!cJSON_AddStringToObject(json, "target", target)) {
        cJSON_Delete(json);
        return false;
    }
    return send_json(session, json);
}

bool ws_client_rec_pcm(uint32_t session, const int16_t *pcm, size_t samples)
{
    // Capture is the sole sender of microphone PCM.
    static uint8_t packet[1 + AUDIO_RECORD_CHUNK_SAMPLES * sizeof(int16_t)] = { RECORD_PCM };
    if (!samples || samples > AUDIO_RECORD_CHUNK_SAMPLES) return false;
    size_t bytes = samples * sizeof(*pcm);
    memcpy(packet + 1, pcm, bytes);
    return send_ready(session, packet, bytes + 1, true);
}

void ws_client_rec_end(uint32_t session, const char *target, bool cancelled)
{
    cJSON *json = event_json("rec_end");
    if (cJSON_AddStringToObject(json, "target", target) && cJSON_AddBoolToObject(json, "cancelled", cancelled)) {
        send_json(session, json);
    } else {
        cJSON_Delete(json);
    }
}

void ws_client_tts_ack(uint32_t session, uint32_t audio_id, bool aborted)
{
    // Media can enqueue from the receive callback or while holding its lock.
    enqueue_session((input_t){ .kind = TTS_ACK, .session = session, .audio_id = audio_id, .aborted = aborted });
}

static void send_input(const input_t *input)
{
    uint32_t session = input->session;
    if (session != atomic_load(&ready_session)) return;
    if (input->kind == TTS_ACK) {
        char text[96];
        int len = snprintf(text, sizeof(text), "{\"ev\":\"tts_ack\",\"state\":\"%s\",\"aborted\":%s,\"audio\":%" PRIu32 "}",
                           input->aborted ? "interrupted" : "done", input->aborted ? "true" : "false", input->audio_id);
        if (len > 0 && (size_t)len < sizeof(text)) send_ready(session, text, len, false);
        return;
    }
    if (input->kind == REJECT_RECORD) {
        ws_client_rec_end(session, input->target, true);
        return;
    }
    bool head = input->kind == INPUT_HEAD;
    const char *event = head ? "head" : input->kind == INPUT_DRAG ? "drag" : input->kind == INPUT_UP ? "up" : "tap";
    cJSON *json = event_json(event);
    bool ok = head ? cJSON_AddStringToObject(json, "action", "pet") &&
                     cJSON_AddNumberToObject(json, "volume", input->x)
                   : cJSON_AddNumberToObject(json, "x", input->x) && cJSON_AddNumberToObject(json, "y", input->y);
    if (ok) send_json(session, json);
    else cJSON_Delete(json);
}

static bool socket_writable(void)
{
    int fd = atomic_load(&socket_fd);
    if (fd < LWIP_SOCKET_OFFSET || fd >= LWIP_SELECT_MAXNFDS) return false;
    fd_set writable, errors;
    FD_ZERO(&writable);
    FD_ZERO(&errors);
    FD_SET(fd, &writable);
    FD_SET(fd, &errors);
    struct timeval zero = { 0 };
    return lwip_select(fd + 1, NULL, &writable, &errors, &zero) > 0 &&
           FD_ISSET(fd, &writable) && !FD_ISSET(fd, &errors);
}

static bool send_telemetry(uint32_t session, bool hello)
{
    if (!hello && media_is_recording()) return false;
    telemetry_t hw = telemetry_read();
    battery_status_t battery = battery_status();
    char pct[12] = "null", mv[16] = "null";
    if (battery.pct >= 0) {
        snprintf(pct, sizeof(pct), "%d", battery.pct);
        snprintf(mv, sizeof(mv), "%u", battery.millivolts);
    }
    char extra[240] = "";
    if (hello) {
        char ip[WIFI_IP_LEN];
        wifi_get_ip(ip);
        int len = snprintf(extra, sizeof(extra), ",\"role\":\"device\",\"device_id\":\"%s\",\"character\":\"%s\","
                           "\"ip\":\"%s\",\"firmware\":\"%s\",\"proto\":2,\"width\":%d,\"height\":%d,"
                           "\"shape\":\"" BOARD_SHAPE "\",\"board\":\"" BOARD_NAME "\"",
                           device_id, wifi_character(), ip, esp_app_get_description()->version, FRAME_WIDTH, FRAME_HEIGHT);
        if (len < 0 || (size_t)len >= sizeof(extra)) return false;
    }
    char text[512];
    int len = snprintf(text, sizeof(text), "{\"ev\":\"%s\"%s,\"free_heap\":%zu,\"min_free_heap\":%zu,"
                       "\"free_psram\":%zu,\"wifi_rssi\":%d,\"rssi\":%d,\"uptime_s\":%" PRIu64 ",\"cpu_pct\":%u,"
                       "\"battery_pct\":%s,\"battery_mv\":%s,\"charging\":%s,\"volume\":%d}",
                       hello ? "hello" : "status", extra, hw.free_heap, hw.min_free_heap, hw.free_psram,
                       hw.wifi_rssi, hw.wifi_rssi, hw.uptime_s, hw.cpu_pct, pct, mv,
                       BOARD_HAS_BATTERY ? (battery.charging ? "true" : "false") : "null", audio_get_volume());
    if (len <= 0 || (size_t)len >= sizeof(text)) return false;
    // Preflight backpressure: the driver's zero-timeout transport failure can close the link.
    if (!hello && (!socket_writable() || media_is_recording())) return false;
    return send_wire(session, text, len, false, hello, hello ? pdMS_TO_TICKS(500) : 0);
}

static const char *field(const cJSON *msg, const char *key)
{
    const char *value = cJSON_GetStringValue(cJSON_GetObjectItemCaseSensitive(msg, key));
    return value ? value : "";
}

static int integer(const cJSON *msg, const char *key, int lo, int hi)
{
    const cJSON *value = cJSON_GetObjectItemCaseSensitive(msg, key);
    return cJSON_IsNumber(value) && value->valuedouble == value->valueint && value->valueint >= lo &&
           value->valueint <= hi ? value->valueint : -1;
}

static void dispatch(const cJSON *msg)
{
    const char *op = field(msg, "op");
    if (!strcmp(op, "rec")) {
        const char *action = field(msg, "action"), *target = field(msg, "target");
        if (!strcmp(action, "stop")) {
            media_record_stop();
        } else if (!strcmp(action, "start") && strlen(target) < MEDIA_TARGET_CAP && !media_record_start(target)) {
            input_t reject = { .kind = REJECT_RECORD };
            snprintf(reject.target, sizeof(reject.target), "%s", target);
            enqueue(reject);
        }
    } else if (!strcmp(op, "tts")) {
        const char *state = field(msg, "state");
        if (!strcmp(state, "start")) {
            const cJSON *audio = cJSON_GetObjectItemCaseSensitive(msg, "audio");
            if (!cJSON_IsNumber(audio) || !(audio->valuedouble >= 0 && audio->valuedouble <= UINT32_MAX)) return;
            uint32_t audio_id = (uint32_t)audio->valuedouble;
            if (audio_id != audio->valuedouble) return;
            tts_accepted = media_tts_start(audio_id);
            if (!tts_accepted) ws_client_tts_ack(ws_client_session(), audio_id, true);
        } else if (!strcmp(state, "end")) {
            if (tts_accepted) media_tts_end();
            tts_accepted = false;
        } else if (!strcmp(state, "abort")) {
            media_interrupt();
            tts_accepted = false;
        }
    } else if (!strcmp(op, "chime")) {
        const char *kind = field(msg, "kind");
        if (!strcmp(kind, "tap")) media_chime(AUDIO_START_TONE);
        else if (!strcmp(kind, "think")) media_chime(AUDIO_THINK_TONE);
        else if (!strcmp(kind, "intercom")) media_chime(AUDIO_HAPPY_TONE);
        else if (!strcmp(kind, "cancel")) media_chime(AUDIO_QUIET_TONE);
    } else if (!strcmp(op, "power")) {
        display_set_brightness(integer(msg, "brightness", 12, 85));
    } else if (!strcmp(op, "volume")) {
        int level = integer(msg, "level", 0, 100);
        if (level >= 0) {
            bool live = cJSON_IsFalse(cJSON_GetObjectItemCaseSensitive(msg, "persist"));
            esp_err_t err = live ? audio_set_volume_live(level) : audio_set_volume(level);
            if (err != ESP_OK) ESP_LOGW(TAG, "Could not set volume");
        }
    }
}

static void receive_pcm(const uint8_t *bytes, size_t len)
{
    if (!len) return;
    if (rx.odd) {
        uint8_t sample[] = { rx.carry, *bytes++ };
        --len;
        rx.odd = false;
        if (!media_tts_pcm(sample, sizeof(sample))) {
            rx.drop = true;
            return;
        }
    }
    size_t even = len - len % 2;
    if (even && !media_tts_pcm(bytes, even)) {
        rx.drop = true;
        return;
    }
    if (len != even) {
        rx.carry = bytes[even];
        rx.odd = true;
    }
}

static void buffer_bytes(const uint8_t *bytes, size_t len, size_t cap)
{
    if (len > cap - rx.used) {
        rx.drop = true;
        return;
    }
    if (len) memcpy(rx_buffer + rx.used, bytes, len);
    rx.used += len;
}

static void receive_binary(const uint8_t *bytes, size_t len)
{
    if (!rx.type && len) {
        rx.type = *bytes;
        if (rx.type == SPEAKER_PCM && tts_accepted) {
            ++bytes;
            --len;
        } else if (rx.type != FRAME_TAG) {
            rx.drop = true;
            return;
        }
    }
    if (rx.type == SPEAKER_PCM) receive_pcm(bytes, len);
    else if (rx.type == FRAME_TAG) buffer_bytes(bytes, len, FRAME_PACKET_CAP);
}

static void receive_frame(const esp_websocket_event_data_t *event)
{
    uint8_t op = event->op_code;
    if (op > 2) return; // interleaved ping/pong/close don't change message state
    if (event->data_len < 0 || event->payload_offset < 0 || event->payload_offset > event->payload_len ||
        event->data_len > event->payload_len - event->payload_offset) {
        rx = (fragment_t){ 0 };
        return;
    }
    // Offset resets for each WS fragment; the message continues on opcode 0.
    if (event->payload_offset == 0 && op) rx = (fragment_t){ .opcode = op };
    if (!rx.opcode) return;
    if (!rx.drop) {
        const uint8_t *bytes = (const uint8_t *)event->data_ptr;
        if (rx.opcode == 1) buffer_bytes(bytes, event->data_len, TEXT_CAP - 1);
        else receive_binary(bytes, event->data_len);
    }
    if (!event->fin || event->payload_offset + event->data_len != event->payload_len) return;
    if (!rx.drop) {
        if (rx.opcode == 1) {
            rx_buffer[rx.used] = '\0';
            cJSON *msg = cJSON_ParseWithLengthOpts((const char *)rx_buffer, rx.used + 1, NULL, true);
            if (cJSON_IsObject(msg)) dispatch(msg);
            cJSON_Delete(msg);
        } else if (rx.type == FRAME_TAG) {
            if (!display_apply_frame(rx_buffer, rx.used)) ESP_LOGD(TAG, "Ignored display frame");
        } else if (rx.odd) {
            media_interrupt();
        }
    }
    rx = (fragment_t){ 0 };
}

static void drop_link(void)
{
    if (atomic_exchange(&socket_session, 0)) {
        atomic_store(&socket_fd, -1);
        atomic_store(&ready_session, 0);
        media_link_lost();
        display_set_link(false);
    }
}

static void on_event(void *arg, esp_event_base_t base, int32_t id, void *data)
{
    (void)arg;
    (void)base;
    if (id == WEBSOCKET_EVENT_CONNECTED) {
        if (!atomic_load(&wifi_online)) return;
        rx = (fragment_t){ 0 };
        tts_accepted = false;
        reconnect_ms = 1000;
        esp_websocket_client_set_reconnect_timeout(client, reconnect_ms);
        display_set_link(true);
        if (++next_session == 0) ++next_session;
        atomic_store(&socket_fd, esp_transport_get_socket(ws_transport));
        atomic_store(&socket_session, next_session);
        if (!atomic_load(&wifi_online)) drop_link();
        xTaskNotifyGive(service);
    } else if (id == WEBSOCKET_EVENT_DATA) {
        receive_frame(data);
    } else if (id == WEBSOCKET_EVENT_DISCONNECTED || id == WEBSOCKET_EVENT_CLOSED) {
        drop_link();
        esp_websocket_client_set_reconnect_timeout(client, reconnect_ms);
        if (reconnect_ms < 30000) reconnect_ms = reconnect_ms > 15000 ? 30000 : reconnect_ms * 2;
    } else if (id == WEBSOCKET_EVENT_FINISH) {
        drop_link();
        atomic_store(&finished, true);
    }
}

static void service_task(void *arg)
{
    (void)arg;
    bool running = false;
    int64_t after_status = 0;
    for (;;) {
        ulTaskNotifyTake(pdTRUE, pdMS_TO_TICKS(running ? 250 : 1000));
        if (running && atomic_exchange(&finished, false)) {
            // FINISH is dispatched before the driver task finishes closing the socket.
            esp_websocket_client_stop(client);
            running = false;
        }
        if (atomic_load(&wifi_online)) {
            if (!running) running = esp_websocket_client_start(client) == ESP_OK;
        } else if (running) {
            esp_websocket_client_stop(client);
            drop_link();
            atomic_store(&finished, false);
            running = false;
        }
        uint32_t session = ws_client_session();
        int64_t now = esp_timer_get_time() / 1000;
        if (session && session != atomic_load(&ready_session)) {
            if (send_telemetry(session, true) && session == ws_client_session()) {
                atomic_store(&ready_session, session);
                after_status = now + STATUS_MS;
                ESP_LOGI(TAG, "Bridge connected");
            }
        } else if (session && now >= after_status) {
            send_telemetry(session, false);
            after_status = now + STATUS_MS;
        }
        input_t input;
        while (xQueueReceive(inputs, &input, 0) == pdPASS) send_input(&input);
    }
}

void ws_client_network_changed(bool online)
{
    atomic_store(&wifi_online, online);
    if (!online) drop_link();
    if (service) xTaskNotifyGive(service);
}

esp_err_t ws_client_init(void)
{
    static char url[sizeof(CONFIG_ECHOEAR_BRIDGE_URL) + 5];
    static const char headers[] = "X-EchoEar-Token: " CONFIG_ECHOEAR_BRIDGE_TOKEN "\r\n";
    const char *base = CONFIG_ECHOEAR_BRIDGE_URL, *host, *scheme;
    if (!strncmp(base, "http://", 7)) {
        host = base + 7;
        scheme = "ws";
    } else if (!strncmp(base, "https://", 8)) {
        host = base + 8;
        scheme = "wss";
    } else {
        return ESP_ERR_INVALID_ARG;
    }
    size_t length = strcspn(host, "/?#@");
    if (!length || (host[length] && strcmp(host + length, "/"))) return ESP_ERR_INVALID_ARG;
    snprintf(url, sizeof(url), "%s://%.*s/ws", scheme, (int)length, host);

    // Own the transport so heartbeat preflight can see the connected socket.
    bool tls = !strcmp(scheme, "wss");
    esp_transport_handle_t parent = tls ? esp_transport_ssl_init() : esp_transport_tcp_init();
    if (!parent) goto fail;
    if (tls) esp_transport_ssl_crt_bundle_attach(parent, esp_crt_bundle_attach);
    ws_transport = esp_transport_ws_init(parent);
    if (!ws_transport) goto fail;
    esp_transport_set_default_port(ws_transport, tls ? 443 : 80);
    const esp_transport_ws_config_t ws_config = {
        .ws_path = "/ws", .headers = headers, .propagate_control_frames = true,
    };
    if (esp_transport_ws_set_config(ws_transport, &ws_config) != ESP_OK) goto fail;
    const esp_websocket_client_config_t config = {
        .uri = url, .ext_transport = ws_transport, .enable_close_reconnect = true,
        .reconnect_timeout_ms = 1000, .network_timeout_ms = 5000, .buffer_size = 4096, .task_stack = 6144,
    };
    rx_buffer = heap_caps_malloc(FRAME_PACKET_CAP, MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    inputs = xQueueCreate(24, sizeof(input_t));
    client = esp_websocket_client_init(&config);
    if (!rx_buffer || !inputs || !client) goto fail;
    if (esp_websocket_register_events(client, WEBSOCKET_EVENT_ANY, on_event, NULL) != ESP_OK ||
        xTaskCreate(service_task, "ws_service", 5120, NULL, 4, &service) != pdPASS) goto fail;
    return ESP_OK;

fail:
    if (client) esp_websocket_client_destroy(client);
    if (ws_transport) esp_transport_destroy(ws_transport);
    if (parent) esp_transport_destroy(parent);
    if (inputs) vQueueDelete(inputs);
    free(rx_buffer);
    client = NULL;
    ws_transport = NULL;
    inputs = NULL;
    rx_buffer = NULL;
    return ESP_ERR_NO_MEM;
}
