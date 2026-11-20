#pragma once

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct {
    float echo_env, mic_gain, spk_gain;
    bool muted;
} echo_suppress_t;

void echo_suppress_process(echo_suppress_t *s, int16_t *mic, int16_t *spk, size_t samples);
