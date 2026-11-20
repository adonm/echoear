#pragma once

#include <stdint.h>

// One update per 10 ms; input is the mean squared amplitude of the mono block.
typedef struct {
    uint32_t quiet[4];
    uint32_t threshold;
    unsigned frames, streak, voiced, silence;
} vad_t;

typedef enum { VAD_CONTINUE, VAD_SEND, VAD_NO_SPEECH } vad_result_t;
vad_result_t vad_update(vad_t *vad, uint32_t energy);
