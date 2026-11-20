#include "vad.h"

enum { CALIBRATION = 20, MIN_RMS = 320, MAX_RMS = 2400, MIN_WORD_FRAMES = 10 };

vad_result_t vad_update(vad_t *v, uint32_t energy)
{
    if (++v->frames <= CALIBRATION) {
        // Quietest four of the first 20 blocks limit bias if speech starts early.
        for (unsigned i = 0; i < 4; ++i) {
            if (!v->quiet[i] || energy < v->quiet[i]) {
                uint32_t replaced = v->quiet[i];
                v->quiet[i] = energy ? energy : 1;
                if (!replaced) {
                    break;
                }
                energy = replaced;
            }
        }
        if (v->frames == CALIBRATION) {
            uint64_t sum = (uint64_t)v->quiet[0] + v->quiet[1] + v->quiet[2] + v->quiet[3];
            uint64_t adaptive = sum * 25 / 16; // square the 2.5× RMS multiplier
            v->threshold = adaptive < MIN_RMS * MIN_RMS ? MIN_RMS * MIN_RMS :
                           adaptive > MAX_RMS * MAX_RMS ? MAX_RMS * MAX_RMS : adaptive;
        }
        return VAD_CONTINUE;
    }

    if (!v->voiced) {
        v->streak = energy >= v->threshold ? v->streak + 1 : 0;
        if (v->streak >= 3) {
            v->voiced = v->streak;
        } else if (v->frames >= 550) {
            return VAD_NO_SPEECH;
        }
    } else if (energy >= v->threshold / 2) {
        ++v->voiced;
        v->silence = 0;
    } else if (++v->silence >= 130) {
        return v->voiced >= MIN_WORD_FRAMES ? VAD_SEND : VAD_NO_SPEECH;
    }
    return VAD_CONTINUE;
}
