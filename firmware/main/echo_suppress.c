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

void echo_suppress_process(echo_suppress_t *s, int16_t *mic, int16_t *spk, size_t samples, int volume)
{
    if (!samples) return;
    // At volume 55 the ES7210 hears about 28% of the digital speaker RMS.
    const float speaker_floor = 550.0f, coupling = 0.51f * volume / 100.0f, double_talk_ratio = 1.15f;
    if (volume == 0) {
        s->echo_env = 0.0f;
        s->mic_gain = s->spk_gain = 1.0f;
        if (s->muted) for (size_t i = 0; i < samples; ++i) mic[i] = 0;
        return;
    }
    // Instant attack; exp(-10/60) per frame covers DMA and room decay.
    s->echo_env = fmaxf(rms(spk, samples), s->echo_env * 0.846f);
    bool echo = s->echo_env >= speaker_floor;
    bool double_talk = echo && !s->muted && rms(mic, samples) > s->echo_env * coupling * double_talk_ratio;
    float want_mic = echo && !double_talk ? 0.08f : 1.0f;
    float want_spk = double_talk ? 0.7f : 1.0f;
    // Interpolate across the frame to avoid clicks on gain changes.
    float mic_start = s->mic_gain > 0 ? s->mic_gain : 1.0f;
    float spk_start = s->spk_gain > 0 ? s->spk_gain : 1.0f;
    s->mic_gain = mic_start + 0.75f * (want_mic - mic_start);
    s->spk_gain = spk_start + 0.35f * (want_spk - spk_start);
    for (size_t i = 0; i < samples; ++i) {
        float at = (float)(i + 1) / samples;
        mic[i] = s->muted ? 0 : (int16_t)(mic[i] * (mic_start + at * (s->mic_gain - mic_start)));
        spk[i] = (int16_t)(spk[i] * (spk_start + at * (s->spk_gain - spk_start)));
    }
}

void call_uplink_gain(int16_t *pcm, size_t samples)
{
    // Apply gain after echo detection without changing the ES7210's note/VAD levels.
    const int32_t knee = 16000, headroom = 15000;
    for (size_t i = 0; i < samples; ++i) {
        int32_t value = (int32_t)pcm[i] * 8;
        int32_t mag = value < 0 ? -value : value;
        if (mag > knee) {
            int32_t excess = mag - knee;
            mag = knee + headroom - headroom * headroom / (headroom + excess);
        }
        pcm[i] = (int16_t)(value < 0 ? -mag : mag);
    }
}
