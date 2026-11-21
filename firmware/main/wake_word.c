#include "wake_word.h"

#include <stdlib.h>
#include <string.h>

#include "audio.h"
#include "esp_wn_models.h"
#include "model_path.h"

extern const uint8_t wake_model[] asm("_binary_wake_model_bin_start");

static srmodel_list_t *models;
static const esp_wn_iface_t *iface;
static model_iface_data_t *model;
static int16_t *chunk;
static size_t used, samples;

bool wake_word_start(void)
{
    if (!models) models = srmodel_load(wake_model);
    const char *name = esp_srmodel_filter(models, ESP_WN_PREFIX, NULL);
    if (!name) return false;
    iface = esp_wn_handle_from_name(name);
    if (!iface) return false;
    model = iface->create(name, DET_MODE_90);
    if (!model) return false;

    int count = iface->get_samp_chunksize(model);
    if (count > 0 && iface->get_samp_rate(model) == AUDIO_RATE && iface->get_channel_num(model) == 1) {
        chunk = malloc((size_t)count * sizeof(*chunk));
        if (chunk) {
            samples = count;
            used = 0;
            return true;
        }
    }
    wake_word_stop();
    return false;
}

void wake_word_stop(void)
{
    if (model) iface->destroy(model);
    model = NULL;
    free(chunk);
    chunk = NULL;
}

void wake_word_reset(void)
{
    if (model) iface->clean(model);
    used = 0;
}

esp_err_t wake_word_poll(bool *detected)
{
    int16_t pcm[AUDIO_10MS];
    esp_err_t err = audio_read_10ms(pcm);
    *detected = false;
    if (err != ESP_OK) return err;
    for (size_t pos = 0; pos < AUDIO_10MS;) {
        size_t count = samples - used;
        if (count > AUDIO_10MS - pos) count = AUDIO_10MS - pos;
        memcpy(chunk + used, pcm + pos, count * sizeof(*chunk));
        pos += count;
        used += count;
        if (used == samples) {
            *detected |= iface->detect(model, chunk) == WAKENET_DETECTED;
            used = 0;
        }
    }
    return ESP_OK;
}
