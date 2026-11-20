#include <assert.h>
#include <stdio.h>
#include "vad.h"

static void blocks(vad_t *vad, unsigned count, uint32_t rms)
{
    while (count--) {
        assert(vad_update(vad, rms * rms) == VAD_CONTINUE);
    }
}

int main(void)
{
    vad_t quiet = { 0 };
    blocks(&quiet, 549, 180);
    assert(!quiet.voiced);
    assert(vad_update(&quiet, 180 * 180) == VAD_SEND);

    vad_t voice = { 0 };
    blocks(&voice, 20, 250);
    assert(voice.threshold == 390625);
    blocks(&voice, 50, 1000);
    uint32_t keep_alive = voice.threshold / 4;
    for (unsigned i = 0; i < 179; ++i) {
        assert(vad_update(&voice, keep_alive - 1) == VAD_CONTINUE);
    }
    assert(voice.silence == 179);
    assert(vad_update(&voice, keep_alive) == VAD_CONTINUE);
    assert(voice.silence == 0);
    blocks(&voice, 179, 250);
    assert(vad_update(&voice, 250 * 250) == VAD_SEND);

    vad_t short_noise = { 0 };
    blocks(&short_noise, 20, 150);
    blocks(&short_noise, 2, 2500);
    blocks(&short_noise, 527, 150);
    assert(!short_noise.voiced);
    assert(vad_update(&short_noise, 150 * 150) == VAD_SEND);

    vad_t word = { 0 };
    blocks(&word, 20, 150);
    blocks(&word, 3, 900);
    blocks(&word, 179, 150);
    assert(vad_update(&word, 150 * 150) == VAD_SEND);

    vad_t intermittent = { 0 };
    blocks(&intermittent, 20, 100);
    blocks(&intermittent, 30, 900);
    blocks(&intermittent, 175, 100);
    blocks(&intermittent, 20, 900);
    blocks(&intermittent, 179, 100);
    assert(vad_update(&intermittent, 100 * 100) == VAD_SEND);
    puts("PASS VAD calibration, 3-frame speech, 180-frame silence, /4 energy keep-alive, and 550-frame quiet send");
}
