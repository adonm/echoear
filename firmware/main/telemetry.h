#pragma once

#include <stddef.h>
#include <stdint.h>

typedef struct {
    size_t free_heap, min_free_heap, free_psram;
    uint64_t uptime_s;
    int wifi_rssi;
    unsigned cpu_pct;
} telemetry_t;

telemetry_t telemetry_read(void);
