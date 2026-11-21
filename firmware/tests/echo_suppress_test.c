#include "echo_suppress.h"

#include <assert.h>
#include <stdio.h>

enum { SAMPLES = 160 };
static int16_t mic[SAMPLES], spk[SAMPLES];

static void frame_at(echo_suppress_t *s, int16_t near, int16_t far, int volume)
{
    for (size_t i = 0; i < SAMPLES; ++i) {
        mic[i] = near;
        spk[i] = far;
    }
    echo_suppress_process(s, mic, spk, SAMPLES, volume);
}

static void frame(echo_suppress_t *s, int16_t near, int16_t far)
{
    frame_at(s, near, far, 55);
}

int main(void)
{
    echo_suppress_t s = { 0 };
    echo_suppress_process(&s, NULL, NULL, 0, 55);
    // Faint far-end room noise must not duck either quiet or loud local speech.
    frame(&s, INT16_MIN, 549);
    for (size_t i = 0; i < SAMPLES; ++i) {
        assert(mic[i] == INT16_MIN && spk[i] == 549);
    }
    for (int i = 0; i < 8; ++i) frame(&s, 400, 500);
    assert(mic[0] == 400 && mic[159] == 400);

    frame(&s, 850, 3200);
    assert(s.echo_env == 3200.0f);
    assert(mic[0] > mic[159] && mic[159] > 0);
    for (int i = 0; i < 6; ++i) frame(&s, 850, 3200);
    assert(mic[159] >= 68 && mic[159] <= 70 && spk[159] == 3200);
    for (int i = 0; i < 6; ++i) frame(&s, 100, 0);
    assert(s.echo_env > 1150 && s.echo_env < 1200 && mic[159] <= 9);
    for (int i = 0; i < 9; ++i) frame(&s, 100, 0);
    assert(s.echo_env < 550 && mic[0] >= 99 && mic[159] >= 99);

    // Normal local speech breaks through even though it is quieter than the
    // digital far-end PCM. Test a louder far-end frame with echo alone too.
    for (int i = 0; i < 8; ++i) frame(&s, 900, 3500);
    frame(&s, 1450, 3500);
    assert(mic[0] < mic[159] && mic[159] > 1100 && spk[0] > spk[159]);
    for (int i = 0; i < 15; ++i) frame(&s, INT16_MIN, 3500);
    assert(mic[159] <= -32760 && spk[159] >= 2450 && spk[159] < 2460);
    for (int i = 0; i < 8; ++i) frame(&s, 2200, 8000);
    assert(mic[159] >= 176 && mic[159] <= 178 && spk[159] > 7900);
    frame_at(&s, 600, 8000, 0);
    assert(s.echo_env == 0 && mic[0] == 600 && mic[159] == 600 && spk[159] == 8000);
    frame_at(&s, 900, 3500, 20);
    assert(mic[159] == 900 && spk[159] < 3500);
    s.muted = true;
    frame(&s, INT16_MAX, INT16_MIN);
    assert(s.echo_env == 32768.0f);
    for (size_t i = 0; i < SAMPLES; ++i) {
        assert(mic[i] == 0 && spk[i] < 0);
    }
    s.muted = false;
    for (int i = 0; i < 6; ++i) frame(&s, INT16_MIN, INT16_MAX);
    assert(mic[159] <= -32760 && spk[159] >= 22936 && spk[159] < 24000);

    int16_t boosted[] = { 0, 400, -1500, 2000, 4000, INT16_MAX, INT16_MIN };
    call_uplink_gain(boosted, sizeof(boosted) / sizeof(*boosted));
    assert(boosted[0] == 0 && boosted[1] == 3200 && boosted[2] == -12000);
    assert(boosted[3] == 16000 && boosted[4] > boosted[3] && boosted[5] > boosted[4]);
    assert(boosted[5] < 31000 && boosted[6] > -31000 && boosted[6] <= -boosted[5]);
    puts("PASS call audio: room noise, echo, release, double-talk, volume, mute and uplink limiting");
}
