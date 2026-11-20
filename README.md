# EchoEar

EchoEar turns an ESP-VoCat v1.2 into a tap-to-talk voice tutor. Each device gets an Adventure Time–style cartoon cat on its round 360×360 display: **Cake**, **Mint**, **Plum**, or **Sunny**. In `v0.6.0`, cats on the same bridge can send each other walkie-talkie messages.

- **Ask with one tap.** Tap the cat or pet the top of its head to start recording. Speak after the tone; voice activity detection (VAD) stops recording when you finish, and a thinking chime confirms that your question was sent. Tap or pet again to stop recording early or interrupt speech. There are no engagement chips or always-listening wake word.
- **Pet, slide, or hold.** The top-of-head capacitive slider uses pads 6 and 7. A pet wakes a sleeping cat; pet again to ask. Slide across the pads or hold either pad to step the speaker volume, or set it with `POST /volume`.
- **Sleep and battery.** After 120 seconds idle, the cat goes to sleep and the display dims. A battery pill reads the BQ27220 fuel gauge and shows charge and charging status.
- **Listen and explore.** Local Kokoro-82M (`af_heart`) starts streaming speech sentence by sentence while later sentences are still being synthesized. The SciShow Kids–style [`echoear` agent](agents/echoear.md) (`opencode/gemini-4-neon#high` by default) can draw interactive diagrams with `btn`, `tap`, and `ask`. It can use `webfetch`; `edit` and `shell` are denied.

```text
EchoEar v1.2 (mic / speaker / LVGL touchscreen)
    | PCM over Wi-Fi                     ^
    v                                     | transcript + reply + diagram + speech
Python LAN bridge :8765 ------------------+
    |-- audio ------> Anomaly Hex 3.4.0  (parakeet_unified_en, Vulkan GPU STT)
    |-- transcript -> OpenCode V2        (SciShow Kids-style echoear agent)
    `-- reply ------> Kokoro-82M          (local CPU TTS, af_heart voice)
```

**Hardware:** EchoEar / ESP-VoCat v1.2 with ESP32-S3, 16 MB flash, 16 MB octal PSRAM, and a USB data cable for the first flash.

## Quickstart

1. **Set up the host.** Use Linux with a systemd user session, Python 3.11+, Hex 3.4.0, OpenCode V2 with access to the configured model, Vulkan drivers, Podman, `uv`, `uvx`, and `curl`.

   ```sh
   hex model devices
   ./scripts/setup-host.sh --gpu-index 0
   ```

   Use `--gpu-index 0` only if index 0 is your intended Vulkan GPU; omit it to keep Hex's device selection. Setup configures Hex for GPU transcription and installs `parakeet_unified_en`, installs `kokoro-onnx` and its model files for local speech synthesis, links the [SciShow Kids-style agent](agents/echoear.md) into OpenCode, and starts `hex.service` and `echoear-bridge.service`.

2. **Configure firmware.** Create the local config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set your Wi-Fi SSID and password, the bridge's LAN URL (`http://<bridge-host>:8765`, without a trailing slash), and `ECHOEAR_BRIDGE_TOKEN` from `~/.config/echoear/bridge.env` (under `$XDG_CONFIG_HOME` if set).

3. **Build and flash over USB.**

   ```sh
   ./scripts/build.sh && ./scripts/flash-usb.sh
   ```

   Pass a port such as `/dev/ttyACM0` to flash just one device; otherwise the script flashes every `/dev/ttyACM*` it finds. The screen and `./scripts/monitor.sh` show the device's IP. Use **Text** to show the conversation and **New** to start over.

4. **Update over Wi-Fi.** Run `./scripts/push-ota.sh echoear-XXXX.local` with your device's name. It builds, uploads to `/ota/upload`, and checks that the device boots from the other OTA partition. Add `--no-build` to upload the existing image. The firmware advertises `echoear-XXXX.local` and `_http._tcp` on port 80 via mDNS. On a Linux host running Avahi, use `avahi-resolve -n echoear-XXXX.local`. If the container cannot resolve `.local` names, run `host-spawn avahi-resolve -n echoear-XXXX.local` and pass the returned IP to the OTA script. You can also use the LAN IP shown on the device.

**Network:** The EchoEar needs TCP access to the bridge host on port **8765**. Your PC needs TCP access to the EchoEar on port **80** for OTA and direct screen pushes. If IoT Wi-Fi is on another VLAN, allow those connections through the router or put both on the same LAN. Use a trusted LAN: HTTP is unencrypted, and firmware images embed local credentials. Never publish the images, `sdkconfig.local`, or `bridge.env`.

## Walkie-talkie intercom

Cats automatically register with the bridge when they connect to Wi-Fi and periodically refresh their registration. Use `GET /api/devices` to see registered cats and whether they've checked in recently. The device's `GET /status` reports its selected peer as `peer`, or `null` if it has none.

- **Speak a message.** Tap to talk and say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone good morning.” The bridge recognizes these direct commands without asking OpenCode. Recipients hear a chime and “From \<Sender\>!”, then the sender's real voice with its volume normalized.
- **Send or reply with one tap.** Open **Text** and tap **Msg \<Peer\>** to record for that cat without saying its name. Incoming messages show a **From \<Sender\>!** Message Card with a **Reply to \<Sender\>** button. Tap it and speak after the tone to send a reply directly. Both buttons use the bridge's `POST /api/intercom` voice route.
- **Ask a cat to write something.** Say “Send Mint a short knock-knock joke” or “Ask Cake a space quiz question.” The bridge tells OpenCode which cat is talking and which peers it knows. The agent can compose a message using an `echoear-message` block; the bridge delivers it with a Message Card and synthesized speech.

The bridge must be able to reach the cats on port **80** to deliver Message Cards and audio.

## API reference

Except for `GET /health` and `GET /status`, send the shared `X-EchoEar-Token` request header. Quote token values in local config and do not include actual tokens in shared examples.

### Bridge — port 8765

| Method and path | Request and response |
| --- | --- |
| `GET /health` | JSON with `ok`, `hex`, `opencode`, `tts`, and `tts_engine`. |
| `POST /api/register` | JSON with `device_id` and `character`; optional `ip` and `port` default to the request's source IP and `80`. Returns `ok` and a `peers` array excluding the registering cat. |
| `GET /api/devices` | Returns `devices`, an array of registered cats with `device_id`, `character`, `ip`, and `online`. |
| `POST /api/voice` | Raw signed 16-bit little-endian mono PCM at 16 kHz (`audio/pcm`) or WAV (`audio/wav`), up to 2 MiB. Set `X-Device-Id` to identify the conversation. Returns the text response fields plus `transcript` and `hex_ms`. |
| `POST /api/text` | JSON `{"text":"Explain a circle","device_id":"my-echoear"}`. Returns `reply`, `screen`, `session_id`, `opencode_ms`, `audio_url`, `audio_streaming`, `audio_bytes`, `sample_rate`, `tts_first_ms`, and `tts_ms`. `screen` and `audio_url` can be `null`. Intercom deliveries include the fields below. |
| `POST /api/intercom` | Sends `audio/pcm` or `audio/wav` directly to the peer in `X-Target`. Set `X-Device-Id` and `X-Character` for the sender. Returns the transcript and delivery fields. |
| `GET /api/audio/<id>` | Fetches reply PCM from `audio_url`. While synthesis is in progress, HTTP/1.1 sends each sentence with `Transfer-Encoding: chunked`. Completed audio is served with `Content-Length`; an evicted clip returns 404. |
| `POST /api/tts` | JSON `{"text":"Hello, curious explorer!"}`. Returns a complete raw PCM clip, or 503 if TTS is unavailable. This endpoint is not sentence-streamed. |
| `POST /api/reset` | JSON `{"device_id":"my-echoear"}`. Clears the device-to-session mapping and returns `{"ok":true}`. |
| `GET /ota/version.json` | Build manifest with `exists`, byte `size`, `sha256`, and `mtime`. |
| `GET /ota/latest.bin` | Streams local `firmware/build/echoear.bin` and sends `X-Firmware-Sha256`. Returns 404 if there is no image. |

For text and reset, `device_id` takes precedence over `X-Device-Id`. If neither is supplied, the bridge uses `echoear`.

Voice, text, and direct intercom requests refresh a known sender's last-seen time and optional `X-Character` name. Only `POST /api/register` can replace a registered address and port. Intercom deliveries report recipient names in `delivered_to` and device IDs in `sent_to` and `failed_to`.

Both audio endpoints return `audio/pcm`: signed 16-bit little-endian mono at 16 kHz, without a WAV header. In a voice or text reply, `tts_first_ms` measures time to generate the first sentence when audio is available; `tts_ms` currently reports the same time. `audio_bytes` is `0` while the total length is unknown. Text and diagrams still work if TTS is unavailable.

### Device — port 80

| Method and path | Request and response |
| --- | --- |
| `GET /status` | JSON with identity, `character`, `peer`, IP, Wi-Fi signal, free memory, firmware version, running partition, `volume`, `battery_pct`, `battery_mv`, `charging`, and `sleeping`. Battery percentage and voltage can be `null`. |
| `POST /display` | Displays a raw screen JSON object (no wrapper), up to 8 KiB and 24 shapes. |
| `POST /display/clear` | Clears the diagram and restores the chat. No body required. |
| `POST /speak` | Queues raw `audio/pcm` in the format above, up to 1.5 MiB. Returns `{"ok":true,"samples":…}` on acceptance or 409 if busy. |
| `POST /volume` | JSON `{"level":65}`. Sets and saves an integer speaker level from 0 to 100. Returns `{"ok":true,"volume":65}`. |
| `POST /ota` | Pulls the latest image from the configured bridge and reboots on success. |
| `POST /ota/upload` | Accepts raw `echoear.bin` as `application/octet-stream`, validates it, and reboots into the new image. |
| `POST /reboot` | Reboots the device. |

### `echoear-screen` cards

The agent appends an optional fenced `echoear-screen` block. The bridge strips the block from the answer and sends its validated object as `screen`. You can send the same object directly to `/display`.

```echoear-screen
{
  "bg": "#0B1020",
  "title": "Radius",
  "subtitle": "Center to edge",
  "shapes": [
    {"t": "circle", "x": 180, "y": 155, "r": 55, "c": "#38BDF8", "w": 3},
    {"t": "line", "x1": 180, "y1": 155, "x2": 235, "y2": 155, "c": "#FACC15", "w": 4},
    {"t": "text", "x": 202, "y": 140, "s": "r", "c": "#F8FAFC"},
    {"t": "btn", "x": 76, "y": 214, "w": 96, "h": 30, "s": "Quick fact", "c": "#38BDF8", "tap": "The diameter is two radii long."},
    {"t": "btn", "x": 188, "y": 214, "w": 96, "h": 30, "s": "Explore", "c": "#C084FC", "ask": "Where can I find a radius at home?"}
  ]
}
```

Shapes render in array order. Coordinates are pixels from the top-left; circles, arcs, and text are centered on `x,y`, while rectangles start there. Colors are `#RRGGBB`. The other primitives are `rect` (`x,y,w,h`, optional corner radius `r`) and `arc` (`x,y,r,a1,a2`, angles in degrees). Circles and rectangles accept `fill: true`; lines, circles, and arcs accept stroke width `w`.

Use `btn` for a pill button with top-left `x,y`, size `w,h`, label `s`, fill color `c`, and optional corner radius `r`. Any shape can use `tap` to show an instant fun fact or quiz answer locally, or `ask` to send a live follow-up to the bridge in the device's conversation. Both actions take plain text up to 120 characters.

An intercom Message Card uses a `btn` with `"reply_to":"Cake"` to start a voice recording addressed to Cake.

### `echoear-message` messages

To deliver a composed message, the agent adds a fenced `echoear-message` block to its ordinary reply. The bridge removes the block from the reply and sends `text` to the named peer in `to`.

```echoear-message
{"to":"Mint","text":"Quick quiz from Cake: which planet is known for its rings?"}
```

## Customize and troubleshoot

Edit [agents/echoear.md](agents/echoear.md) to change the prompt and diagram guidance. To choose a different model, set `ECHOEAR_OPENCODE_MODEL` in `bridge.env` and restart the bridge.

Check the host with `systemctl --user status hex.service echoear-bridge.service` and `curl -fsS http://localhost:8765/health`. Run `python3 bridge/test_bridge.py` for the live Hex/OpenCode smoke test. It uses Flite to generate speech, or accepts a spoken test WAV with `--wav`.
