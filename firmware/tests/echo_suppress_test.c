#include "echo_suppress.h"

#include <assert.h>
#include <stdio.h>
#include <stdlib.h>

enum { SAMPLES = 160 };
static int16_t mic[SAMPLES], spk[SAMPLES];

static void frame(echo_suppress_t *s, int16_t near, int16_t far)
{
    for (size_t i = 0; i < SAMPLES; ++i) {
        mic[i] = near;
        spk[i] = far;
    }
    echo_suppress_process(s, mic, spk, SAMPLES);
}

int main(void)
{
    echo_suppress_t s = { 0 };
    echo_suppress_process(&s, NULL, NULL, 0);
    frame(&s, INT16_MIN, 119);
    for (size_t i = 0; i < SAMPLES; ++i) {
        assert(mic[i] == INT16_MIN && spk[i] == 119);
    }
    frame(&s, 900, 2000);
    assert(s.echo_env == 2000.0f);
    assert(mic[0] > mic[159] && mic[159] > 0);
    for (int i = 0; i < 6; ++i) frame(&s, 900, 2000);
    assert(mic[159] >= 17 && mic[159] <= 19 && spk[159] == 2000);
    for (int i = 0; i < 18; ++i) frame(&s, 200, 0);
    assert(s.echo_env > 710 && s.echo_env < 760 && mic[159] <= 5);
    for (int i = 0; i < 40; ++i) frame(&s, 200, 0);
    assert(s.echo_env < 120 && mic[0] == 200 && mic[159] == 200);

    for (int i = 0; i < 8; ++i) frame(&s, 900, 2000);
    frame(&s, 5000, 2000);
    assert(mic[0] < mic[159] && mic[159] > 3750 && spk[0] > spk[159]);
    for (int i = 0; i < 15; ++i) frame(&s, INT16_MIN, 2000);
    assert(mic[159] <= -32760 && spk[159] >= 1400 && spk[159] < 1410);
    s.muted = true;
    frame(&s, INT16_MAX, INT16_MIN);
    assert(s.echo_env == 32768.0f);
    for (size_t i = 0; i < SAMPLES; ++i) {
        assert(mic[i] == 0 && spk[i] < 0);
    }
    s.muted = false;
    for (int i = 0; i < 6; ++i) frame(&s, INT16_MIN, INT16_MAX);
    assert(abs(mic[159]) < 670 && spk[159] > 31000);
    puts("PASS echo suppression: silence, echo, release, double-talk, mute and full scale");
}
