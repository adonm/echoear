# Bridge endpointing

## Synthetic shadow replay

This table is the historical Hex baseline. EchoEar now decodes whole clips
in-process with Parakeet Vulkan; see [the STT bake-off](stt-speed.md).

Nine Kokoro-generated clips, one synthetic voice, no real recordings. The fixture
generator inserts 0.7–4.0 s gaps and estimates the true end from each generated
clause's last audible block. Names and numbers are scored as exact normalized
markers; written and spoken numbers are accepted. This is not a field benchmark.

| Replay path | Premature stops | End → endpoint p50 / p95¹ | End → transcript p50 / p95¹ | Name errors | Number errors |
| --- | ---: | ---: | ---: | ---: | ---: |
| Board RMS + whole-clip Hex | 6/9 | 1,500 / 1,500 ms | 1,558 / 1,580 ms | 2/2 | 2/3 |
| Browser RMS + whole-clip Hex | 6/9 | 1,510 / 3,000 ms | 1,566 / 3,084 ms | 2/2 | 2/3 |
| Silero, no partials + whole-clip Hex | 0/9 | 4,550 / 4,570 ms | 4,650 / 5,159 ms | 1/2 | 0/3 |
| Silero, scripted text hints (not live) | 0/9 | 4,540 / 4,570 ms | — | — | — |

¹ Endpoint uses the audio clock. Transcript adds the measured local Hex call;
neither includes a real stop acknowledgement. Percentiles exclude premature
stops (legacy n=3, Silero n=9). With scripted text hints,
the two complete timer requests ended at 960 and 920 ms. A wider set of fast
request prefixes cut 3/9 fixtures early; only complete timer requests use the
fast path now. Unfinished requests, stories, and unknown text wait 4.5 s.

The host's Hex 3.4.0 `/streams` create returned HTTP 503 on the probe; no live
partials or latency could be measured. Configured whole-clip Hex succeeded for
all nine Silero captures. The no-partial fallback trades longer silence for
fewer cutoffs. This run does not demonstrate faster production responses.

```sh
source scripts/go-bin.sh
GO_BIN=$(resolve_go)
cd bridge
ECHOEAR_REPLAY_SILERO=/path/to/silero_vad_v5.onnx \
ECHOEAR_REPLAY_KOKORO=/path/to/kokoro-multi-lang-v1_0 \
ECHOEAR_BAKEOFF_MODEL="$stage/models/parakeet-unified-en-0.6b-Q8_0.gguf" \
CGO_CFLAGS="-I$stage/native/include" CGO_LDFLAGS="-L$stage/native/lib" \
  "$GO_BIN" test -tags parakeet -v -run '^TestEndpointReplay$'
```

Use the `stage` prefix from [the STT build](stt-speed.md#build-and-reproduction).
Replay prints aggregate scores, not transcripts, and saves no clips.
Set `ECHOEAR_REPLAY_SPECULATIVE=1` to also run the experimental 500/700 ms
pause probes. Those probes are not enabled in production.

## Enablement and wire contract

- Off by default. Set `ECHOEAR_SILERO_VAD_MODEL` to the local Silero v5 ONNX
  path (SHA-256 `6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396`).
  The bridge validates the file and never downloads it. Neither STT nor TTS
  model selection changes.
- A device offers `caps.bridge_endpoint:true` in `hello`. Only when configured
  and offered does an untargeted Talk or Say more action (or an enabled wake word)
  start with
  `{"op":"rec","action":"start","target":"","endpoint":"bridge"}`.
  Audio is the existing `0x01` + 16 kHz mono s16le PCM between `rec_start`
  and `rec_end`. The bridge sends `rec stop` to commit at its endpoint and
  gives the client up to 450 ms for `rec_end`; `cancelled:true` discards the
  capture. The bridge has a 7 s no-speech limit (audio time) and a 60 s
  wall-clock limit. Clients retain their own ~8 s silence / 60 s sample
  watchdog. Clients without the cap, unconfigured bridges, targeted intercom,
  and call audio keep their old paths.
- `/app` also offers the cap. It sends `{"ev":"rec_request"}` on Talk only
  after receiving `{"op":"capabilities","bridge_endpoint":true}`; otherwise
  it starts legacy capture. It closes the mic on bridge stop, Cancel, or
  disconnect. No capture is silently reopened. Send asks to commit the current
  clip, and Cancel discards it without a prompt.
- The per-device capture worker owns VAD, accumulation, and final transcription
  off the hub lock. It commits after the conservative silence threshold
  or manual Send, and decodes once on the local GPU. There are no live text
  hints or stream leases. PCM remains in memory. Diagnostics expose
  `speech_end_to_commit_ms` (estimated acoustic end → final transcription).
- A user can select **Say more** while thinking. Recording holds unpublished
  audio until the mic closes. It does not cancel the in-flight prompt. The
  bridge locally transcribes and queues one normal follow-up through the
  existing gate, after the current reply. Explicit Stop still interrupts and
  discards a queued follow-up.

## Steering is not enabled

Disposable OpenCode 2.3.2 sessions accepted `delivery:"queue"` and
`delivery:"steer"` and returned distinct input IDs. The default and configured
EchoEar model probes both failed with provider HTTP 403. They did not establish
successful mid-turn delivery, which assistant reply correlates to the steered
input, or the race with a tool starting just before admission. The bridge does
not send `steer` or retry an admission on that evidence. The normal follow-up
path is tested with fake OpenCode; no production cat session received a probe.
