#include "echo_suppress.h"

#include <math.h>

static float rms(const int16_t *pcm, size_t samples)
{
    uint64_t energy = 0;
    for (size_t i = 0; i < samples; ++i) {
        int32_t sample = pcm[i];
        energy += sample * sample;
    }
    return sqrtf((float)energy / samples);
}

void echo_suppress_process(echo_suppress_t *s, int16_t *mic, int16_t *spk, size_t samples)
{
    if (!samples) return;
    const float floor = 120.0f, double_talk_ratio = 1.25f;
    // Instant attack; exp(-10/180) per frame covers DMA and room decay.
    s->echo_env = fmaxf(rms(spk, samples), s->echo_env * 0.946f);
    bool echo = s->echo_env >= floor;
    bool double_talk = echo && !s->muted && rms(mic, samples) > s->echo_env * double_talk_ratio;
    float want_mic = echo && !double_talk ? 0.02f : 1.0f;
    float want_spk = double_talk ? 0.7f : 1.0f;
    // Interpolate across the frame to avoid clicks on gain changes.
    float mic_start = echo ? s->mic_gain : 1.0f;
    float spk_start = s->spk_gain > 0 ? s->spk_gain : 1.0f;
    s->mic_gain = mic_start + 0.75f * (want_mic - mic_start);
    s->spk_gain = spk_start + 0.35f * (want_spk - spk_start);
    for (size_t i = 0; i < samples; ++i) {
        float at = (float)(i + 1) / samples;
        mic[i] = s->muted ? 0 : (int16_t)(mic[i] * (mic_start + at * (s->mic_gain - mic_start)));
        spk[i] = (int16_t)(spk[i] * (spk_start + at * (s->spk_gain - spk_start)));
    }
}
