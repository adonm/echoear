# EchoEar firmware 0.9.5

Deploy a bridge that sends `hello_ok` before installing this image. Older
firmware ignores the acknowledgement and still works on the new bridge.
Both boards keep their existing partition layout and 4 MiB OTA slots.

## Wake word

Both boards support **“Hi ESP”**, off by default. An authenticated, unscoped
bridge WebSocket observer can send
`{"ev":"wake_word","device_id":"<id>","enabled":true}` (or `false`). The
bridge sends `{"op":"wake_word","enabled":true,"device_id":"<id>"}` to the
board and echoes the setting to the observer. The setting lasts for that
device connection. There is no board menu or browser control in this release.

The media task reads 16 kHz mono only while opted in and idle. The detector has
no audio network output or pre-roll. On detection it wakes the panel and sends
`{"ev":"wake_word"}`. The bridge requests the normal Talk recording, which
acknowledges with `rec_start`, plays the confirmation tone, drains the speaker
tail and renders the listening state. Calls, replies and recording take
precedence over recognition.

[ESP-SR 2.4.0][esp-sr] supports ESP-IDF ≥5.0 and builds on the EchoEar IDF 5.5
image. The bundled WakeNet9 `wn9_hiesp` model is fixed vocabulary and packed
into the application so OTA installs it atomically. It adds about 284 KiB of
model data plus inference code. [Espressif's benchmarks][benchmark] report
16 KiB internal RAM, 324 KiB PSRAM and roughly 9% of one core for **two-channel**
WakeNet9. Use those as a rough budgeting proxy, not a measured figure for
EchoEar's mono detector. When disabled, the detector does not allocate its
inference workspace or run; a small model descriptor can remain resident after
first use.

Changing the cat name does not change the phrase. To add a name, choose a
distinct phrase such as “Hey Cake” and test it against noise, normal
conversation, similar words, the board's own speaker, and different speakers
and distances. [Espressif's custom training][custom] can produce a replacement
model, including a vendor TTS option (one-time customization fee). Check
phrase-specific commercial and trademark terms before distribution. The
alternative [microWakeWord][mww] training pipeline and TFLite Micro support
local inference and custom training without a vendor engagement; the training
code is Apache-2.0. Plan for generated and real positive samples, hard
negatives, threshold tuning and held-out on-board evaluation. A representative
published v2 model (“Okay Nabu”) is 60,264 bytes with a 26,080-byte tensor
arena, excluding features, ESP-NN/TFLite Micro code, task and stacks. EchoEar
does not package that model or runtime.

ESP-SR uses the [ESPRESSIF MIT license](THIRD_PARTY_NOTICES.md), whose grant
is for Espressif products, not an unrestricted MIT license.

## Call echo and transport

Calls retain `echo_suppress.c`. VoCat's and BOX-3's BSPs leave ES7210
`mic_selected` unset; the codec selects MIC1 and MIC2. EchoEar opens Philips
stereo at 16 kHz (not TDM), takes RX slot 0 as mono, and duplicates output
samples into both ES8311 TX slots. The [VoCat 1.2][vocat-sch] and [BOX-3][box-sch]
schematics both route ES8311 DAC output to ES7210 MIC3, but MIC3 is not enabled
in the current stream. RX slot 1 is the other microphone. The next chunk
submitted to TX is not a valid reference either: up to six 240-frame
descriptors (90 ms) separate software timing from actual speaker output, and
TX underruns can insert zeros.

Enable MIC1/2/3 on ES7210 in a hardware test build and prove the TDM layout
(the codec driver also supports reading packed 16-bit TDM through two 32-bit
RX slots). Validate the shared I2S clocks and ES8311 speaker format, then measure
MIC3 gain, clipping and alignment with an impulse/chirp at several volumes.
Test [ESP-SR AFE][afe] in `MR` or `MMR` mode against single- and double-talk
recordings. Changing shared RX/TX framing affects recording and playback as
well as calls; 0.9.5 does not make that change without hardware validation.
Keep the fallback when reference, allocation or convergence fails. The
[vendor benchmark][benchmark] for `MR` voice-communication AFE
is about 49–91 KiB internal RAM, 820–822 KiB PSRAM and 35–37% of one core
across feed/fetch. These are estimates, not EchoEar measurements. WakeNet
runs by itself outside calls so it does not require an AFE or speaker loopback.

Under network backpressure, firmware drops call PCM that was replaced, is too
old or cannot start writing before it reaches the wire. It reports
`call_tx_drops` in both WS telemetry and device `/status`. A real transport
fault or a partially emitted frame can still reconnect; dropping the remainder
of a frame on TCP/TLS would corrupt the subsequent stream.

## Build footprint

| Board | OTA image | Free in 4 MiB slot | Static D/IRAM | Explicit PSRAM buffers |
| --- | ---: | ---: | ---: | ---: |
| VoCat | 1,652,464 B | 2,541,840 B | 152,203 B | ~1.50 MiB |
| BOX-3 | 1,673,568 B | 2,520,736 B | 151,359 B | ~1.30 MiB |

The PSRAM estimates count the two frame buffers, TTS and record buffers.
With the 324 KiB WakeNet proxy budget, those total about 1.81 MiB on VoCat
and 1.61 MiB on BOX-3 before all other allocations and reservations. On the
16 MiB PSRAM board variants, that leaves roughly 14.19/14.39 MiB *before* those
other costs, not a measurement of free memory. The linker reported 189,557 B
and 190,401 B of unallocated D/IRAM respectively, also before runtime use.
Both builds produced zero compiler warnings; initial IDF configuration emitted
Kconfig reconfigure warnings. `./scripts/check.sh` passed.

## Wire contract

- Device hello stays `proto:2`. It adds `caps.wake_word` and
  `caps.bridge_endpoint` (true when media is ready), `caps.pointer_rev:true`,
  and `caps.panel_sleep` (true on VoCat, false on BOX-3).
- Accepted device hello receives `{"op":"hello_ok"}`. A pending image needs
  local audio initialization and that acknowledgement within 45 seconds.
  Device `/status` reports `ws_connected` only after an acknowledgement
  and reports `ota_pending` until rollback is cancelled. `push-ota.sh`
  checks the image's embedded version, new OTA slot and bridge connectivity.
- `tap`, `drag` and `up` carry `rev_lo` (unsigned 16 bit). Taps carry the
  newest fully painted revision at touch-down. The bridge rejects native
  Stop, Talk, Reply, Send/Cancel, Call, Answer, Decline, Hang Up and Mute
  actions unless the revision matches the last native scene enqueued.
  Legacy clients that do not advertise `pointer_rev` may omit the field.
- `{"op":"rec","action":"start","endpoint":"bridge","target":"..."}` uses
  bridge-directed endpointing, preserving the `rec_start`, `0x01` PCM and
  `rec_end` sequence. A `rec` stop ends capture; otherwise 8 seconds of
  silence after initial calibration or 60 seconds of samples end it. Missing
  `endpoint:"bridge"` retains device VAD. Bridge endpoint selection is made by
  the turn-taking bridge, not by the detector.
- `power` retains the existing brightness field. Brightness 12 now switches
  off the VoCat panel and backlight. Touch interrupts wake it with a slower
  polling fallback while dark (120 ms vs. 30 ms). Wi-Fi/HTTP/WS stay active;
  firmware does not force light sleep. BOX-3 still dims instead.

## On-device validation required

1. On both boards, verify default off and off after reconnect, enabling and
   disabling, loud/quiet wake detection, false accepts, latency, chirp and
   visible listening. Capture a WS trace to verify detector silence before
   `rec_start` and test pause/resume across TTS, calls and recordings. Measure
   idle/active CPU and internal heap and PSRAM with wake on/off.
2. Verify successful OTA confirmation with the updated bridge and rollback
   with no bridge acknowledgement, a network failure, or failed audio init.
   Confirm that a pending boot and an old version on a flipped slot are not
   accepted by the updater.
3. Hold a tap while changing the UI, and repeat during call transitions and
   Stop confirmation. Verify fresh actions, drag/up and older firmware.
4. Constrain TCP/TLS uplink on both boards mid-call. Verify that
   `call_tx_drops` rises without a disconnect during backpressure, and that a
   true broken link reconnects. Check mute and far-end/double-talk with the
   existing echo fallback at different speaker volumes.
5. Measure VoCat idle current with the panel dark and wake on/off, and
   check quick taps, touch wake, incoming calls and OTA reachability while
   dark. Verify BOX-3 still dims and wakes without blanking the panel.
6. Exercise bridge stop, cancel, silence watchdog, 60 s cap and normal VAD
   against the integrated bridge on both boards.

[esp-sr]: https://components.espressif.com/components/espressif/esp-sr/versions/2.4.0/dependencies
[benchmark]: https://docs.espressif.com/projects/esp-sr/en/latest/esp32s3/benchmark/README.html
[custom]: https://docs.espressif.com/projects/esp-sr/en/latest/esp32s3/wake_word_engine/ESP_Wake_Words_Customization.html
[mww]: https://github.com/OHF-Voice/micro-wake-word
[afe]: https://docs.espressif.com/projects/esp-sr/en/latest/esp32s3/audio_front_end/README.html
[vocat-sch]: https://dl.espressif.com/AE/esp-dev-kits/ESP-VoCat_SCH_V1_2.pdf
[box-sch]: https://github.com/espressif/esp-box/blob/master/hardware/SCH_ESP32-S3-BOX-3_V1.0/SCH_ESP32-S3-BOX-3-MB_V1.1_20230808.pdf
