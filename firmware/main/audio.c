#include "audio.h"

#include <math.h>

#include "bsp/esp_vocat.h"
#include "esp_check.h"
#include "esp_heap_caps.h"

enum { CHANNELS = 2, FRAMES = AUDIO_RATE / 100, MAX_FRAMES = AUDIO_RATE * AUDIO_MAX_SECONDS };

static const char *TAG = "audio";
static esp_codec_dev_handle_t mic;
static esp_codec_dev_handle_t speaker;
static int16_t *recording;

esp_err_t audio_init(void)
{
    const i2s_std_config_t i2s = {
        .clk_cfg = I2S_STD_CLK_DEFAULT_CONFIG(AUDIO_RATE),
        .slot_cfg = I2S_STD_PHILIPS_SLOT_DEFAULT_CONFIG(I2S_DATA_BIT_WIDTH_16BIT, I2S_SLOT_MODE_STEREO),
        .gpio_cfg = {
            .mclk = BSP_I2S_MCLK, .bclk = BSP_I2S_SCLK, .ws = BSP_I2S_LCLK,
            .dout = BSP_I2S_DOUT, .din = BSP_I2S_DSIN,
        },
    };
    ESP_RETURN_ON_ERROR(bsp_i2c_init(), TAG, "Init codec I2C");
    ESP_RETURN_ON_ERROR(bsp_audio_init(&i2s), TAG, "Init duplex I2S");
    mic = bsp_audio_codec_microphone_init();
    speaker = bsp_audio_codec_speaker_init();
    ESP_RETURN_ON_FALSE(mic && speaker, ESP_FAIL, TAG, "Init mic and speaker");

    esp_codec_dev_sample_info_t format = {
        .sample_rate = AUDIO_RATE, .channel = CHANNELS, .bits_per_sample = 16,
    };
    ESP_RETURN_ON_ERROR(esp_codec_dev_set_out_vol(speaker, 65), TAG, "Set speaker volume");
    ESP_RETURN_ON_ERROR(esp_codec_dev_open(speaker, &format), TAG, "Open ES8311");
    ESP_RETURN_ON_ERROR(esp_codec_dev_open(mic, &format), TAG, "Open ES7210");
    int16_t probe[FRAMES * CHANNELS];
    ESP_RETURN_ON_ERROR(esp_codec_dev_read(mic, probe, sizeof(probe)), TAG, "Read initial I2S frames");

    recording = heap_caps_malloc(MAX_FRAMES * sizeof(*recording), MALLOC_CAP_SPIRAM | MALLOC_CAP_8BIT);
    ESP_RETURN_ON_FALSE(recording, ESP_ERR_NO_MEM, TAG, "Allocate recording in PSRAM");
    ESP_LOGI(TAG, "ES7210 mic and ES8311 speaker open: %u Hz, 16-bit stereo I2S; channel 0 to mono",
             AUDIO_RATE);
    return ESP_OK;
}

void audio_tone(audio_tone_t tone)
{
    const float frequency[] = { 740.0f, 520.0f, 880.0f };
    const int tone_frames = AUDIO_RATE * 60 / 1000;
    const int fade_frames = AUDIO_RATE * 5 / 1000;
    const float step = 2.0f * (float)M_PI * frequency[tone] / AUDIO_RATE;
    int16_t samples[FRAMES * CHANNELS];

    // The BSP queues up to 6 × 240 frames (90 ms); send silence to play the tone
    // out before starting capture or waiting on the bridge.
    for (int offset = 0; offset < tone_frames + AUDIO_RATE * 100 / 1000; offset += FRAMES) {
        for (int i = 0; i < FRAMES; ++i) {
            int n = offset + i;
            int edge = n < tone_frames - n ? n : tone_frames - n;
            float gain = edge <= 0 ? 0.0f : edge < fade_frames ? (float)edge / fade_frames : 1.0f;
            int16_t sample = gain > 0.0f ? (int16_t)(4800.0f * gain * sinf(n * step)) : 0;
            samples[2 * i] = samples[2 * i + 1] = sample;
        }
        if (esp_codec_dev_write(speaker, samples, sizeof(samples)) != ESP_CODEC_DEV_OK) {
            ESP_LOGW(TAG, "Could not play confirmation tone");
            return;
        }
    }
}

esp_err_t audio_record(bool (*stop_requested)(void), const int16_t **pcm, size_t *bytes)
{
    int16_t stereo[FRAMES * CHANNELS];
    // Drop the already-buffered RX frames, including any containing the start tone.
    for (int i = 0; i < 6 * 240 / FRAMES; ++i) {
        ESP_RETURN_ON_ERROR(esp_codec_dev_read(mic, stereo, sizeof(stereo)), TAG, "Drain mic DMA");
    }

    size_t samples = 0;
    while (samples < MAX_FRAMES && !stop_requested()) {
        ESP_RETURN_ON_ERROR(esp_codec_dev_read(mic, stereo, sizeof(stereo)), TAG, "Read mic");
        // The BSP uses Philips stereo; the ES7210 defaults to MIC1 and MIC2 (no TDM).
        for (size_t i = 0; i < FRAMES; ++i) {
            recording[samples++] = stereo[CHANNELS * i];
        }
    }
    *pcm = recording;
    *bytes = samples * sizeof(*recording);
    return ESP_OK;
}
