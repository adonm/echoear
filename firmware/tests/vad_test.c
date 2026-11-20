#include <assert.h>
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
    assert(vad_update(&quiet, 180 * 180) == VAD_NO_SPEECH);

    vad_t voice = { 0 };
    blocks(&voice, 20, 250);
    assert(voice.threshold == 390625);
    blocks(&voice, 50, 1000);
    blocks(&voice, 129, 250);
    assert(vad_update(&voice, 250 * 250) == VAD_SEND);

    vad_t short_noise = { 0 };
    blocks(&short_noise, 20, 150);
    blocks(&short_noise, 2, 2500);
    blocks(&short_noise, 527, 150);
    assert(vad_update(&short_noise, 150 * 150) == VAD_NO_SPEECH);

    vad_t word = { 0 };
    blocks(&word, 20, 150);
    blocks(&word, 10, 900);
    blocks(&word, 129, 150);
    assert(vad_update(&word, 150 * 150) == VAD_SEND);

    vad_t pop = { 0 };
    blocks(&pop, 20, 150);
    blocks(&pop, 9, 2500);
    blocks(&pop, 129, 150);
    assert(vad_update(&pop, 150 * 150) == VAD_NO_SPEECH);

    vad_t intermittent = { 0 };
    blocks(&intermittent, 20, 100);
    blocks(&intermittent, 30, 900);
    blocks(&intermittent, 90, 100);
    blocks(&intermittent, 20, 900);
    blocks(&intermittent, 129, 100);
    assert(vad_update(&intermittent, 100 * 100) == VAD_SEND);
}
