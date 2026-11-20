# EchoEar

EchoEar turns an ESP-VoCat v1.2 into a tap-to-talk tutor with interactive diagrams, intercom, and a browser mirror.

```text
EchoEar <-- /ws: keyed scenes and PCM --> Go bridge :8765 <-- /app --> Browser
                                           |-- Hex      (speech to text)
                                           |-- OpenCode (reply deltas over SSE)
                                           `-- Kokoro   (persistent FP32 worker)
```

**Target:** EchoEar / ESP-VoCat v1.2 (ESP32-S3, 16 MB flash and 16 MB PSRAM).
Phase 2 (`v0.7.0`) uses a 1.28 MB thin client: a pure-C 360×360 RGB565
rasterizer replaces LVGL, applies keyed scene patches, and draws a fallback
face offline. `esp_websocket_client` carries binary audio over Wi-Fi.

## Quickstart

1. On Linux, install Go 1.26, Python 3.11+, Hex 3.4.0, OpenCode V2, Vulkan
   drivers, Podman, `uv`/`uvx`, and `curl`. Use a systemd user session.

   ```sh
   hex model devices
   ./scripts/setup-host.sh --gpu-index 0
   ```

   Pass `--gpu-index 0` only if it selects your intended Vulkan GPU. Setup builds
   the Go bridge, installs Hex's `parakeet_unified_en` and FP32 `kokoro-v1.0.onnx`,
    links the [EchoEar agent](agents/echoear.md), registers its local MCP server,
    and starts both services. The default model is
    `opencode/gemini-4-hydrogen#low`; set `ECHOEAR_OPENCODE_MODEL` in
    `bridge.env` to override it.

2. Create the private firmware config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set `CONFIG_ECHOEAR_WIFI_SSID`, `CONFIG_ECHOEAR_WIFI_PASSWORD`,
   `CONFIG_ECHOEAR_BRIDGE_URL` (`http://<bridge-host>:8765`, no trailing slash), and
   `CONFIG_ECHOEAR_BRIDGE_TOKEN` (copy `ECHOEAR_BRIDGE_TOKEN` from
   `~/.config/echoear/bridge.env`, or use `$XDG_CONFIG_HOME`).
   The build uses the ESP-IDF 5.5 container and regenerates `build/sdkconfig`
   from `firmware/sdkconfig.defaults` and `firmware/sdkconfig.local`.

3. Build and flash. Omit the port to flash every detected `/dev/ttyACM*`.

   ```sh
   ./scripts/build.sh
   ./scripts/flash-usb.sh /dev/ttyACM0
   ./scripts/monitor.sh /dev/ttyACM0
   ```

4. Update via `./scripts/push-ota.sh <ip|host>`; add `--no-build` to reuse the
   image. Direct OTA works without the bridge and reads the token from the
   environment or `bridge.env`. The board confirms the new image after display
   initialization and Wi-Fi DHCP; the script checks the running partition.

5. Run `./scripts/check.sh` for Go vet, race tests, and host VAD and graphics
   tests (GCC, plus cJSON development files or the cached ESP-IDF image).
   Add `--live` to exercise the running services; ShellCheck runs if installed.

## Controls and intercom

- Tap the cat or pet its head, then speak after the tone. Voice activity
  detection stops recording at silence; tap or pet again to stop early or interrupt speech.
- Slide across the head pads to change volume, or hold a pad for one step.
  The display dims after 120 seconds idle unless a diagram is open; tap or pet to wake.
- **Text** opens the peer drawer, where **New** starts a fresh conversation and
  **Voice** switches between Emma and Fable. **Cat** closes a diagram.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” The bridge bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or **Text → Msg \<Peer\>** to record directly to that cat.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  the `intercom` tool to deliver a card and synthesized speech.

## Browser dashboard

Open `http://<host-lan-ip>:8765/app` on a trusted LAN and enter the bridge
token. Select a physical EchoEar to mirror and control it, or use
**New Virtual Cat** to create a named client that persists across restarts.
The Chat panel loads the cat's conversation history across sessions and
updates live over `/ws`. Use **Voice** under the screen for one-click Emma or
Fable, or select another UK voice. The choice is saved per device in `voices.json`.
Mint defaults to Fable. Other cats default to Emma unless `ECHOEAR_TTS_VOICE`
sets a different fallback (including `af_heart` for US English).

Plain LAN HTTP supports the SVG mirror, text chat, and audio playback.
Microphone access requires HTTPS or localhost. For a remote phone, install
the [OpenTunnel CLI](https://opentunnel.xyz) on the bridge host. If it is
outside the bridge service's `PATH` and `~/.local/bin`, set
`ECHOEAR_OPENTUNNEL_BIN` to its path in `bridge.env`. Restart the bridge
with `systemctl --user restart echoear-bridge.service` so it finds the CLI.
In the dashboard, select a Virtual Cat, choose **Create Tunnel** to start
its route, then **Copy Share Link**. The HTTPS link opens the same cat and
conversation as the LAN dashboard, with mic, screen, and intercom to online
EchoEars. The per-cat token does not open the LAN dashboard, admin routes, or
other cats' chats. Treat the link as a password and use **Close Tunnel** to revoke it.

## OpenCode MCP

`scripts/setup-host.sh` uses `opencode mcp add --global` to register
`mcp.servers.echoear` as a local server in `~/.config/opencode/opencode.jsonc`
(or `$XDG_CONFIG_HOME/opencode/opencode.jsonc`). It reuses an existing
`opencode.json` if OpenCode selects it and preserves unrelated settings.
`opencode mcp list` shows whether `echoear` is connected.

OpenCode runs the installed `echoear-bridge mcp` stdio proxy, which forwards
requests to the bridge's authenticated `POST /mcp` on loopback. The proxy
reads the token from its environment or the private `bridge.env`; the
OpenCode config contains only the executable and its argument, no secrets.
Code Mode exposes `tools.echoear.devices`, `snapshot`, `scene`, `patch`,
`speak`, `chime`, `intercom`, and `wait_event` under `tools.echoear`.
Scenes and patches support declarative `tap` and `cycle` reactions, named `states`,
and delayed `after_ms`/`then` feedback. The bridge runs these locally without
holding an OpenCode turn open; only an explicit `ask` starts a new turn.

## API

HTTP API requests use `X-EchoEar-Token` except `/health`, `/app`, and `/status`.
The `/ws` hub sends keyed `scene` and `patch` JSON. Binary frames carry 16 kHz
signed 16-bit little-endian mono PCM (`0x01` mic upload, `0x02` playback). HTTP voice
inputs also accept WAV, up to 2 MiB. Kokoro starts synthesizing sentences
while OpenCode streams its reply over SSE.

| Bridge · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Hex, OpenCode, and TTS readiness |
| `GET /ws`, `GET /app` | WebSocket hub and embedded browser UI |
| `POST /api/register` | `device_id`, `character`, optional `ip` and `port` → other `peers`; only registration can replace an existing address |
| `GET /api/devices` | `devices` with `device_id`, `character`, `ip`, `online`, `voice` |
| `GET /api/devices/{id}/messages` | Chronological chat across sessions and bridge events for a device |
| `POST /api/devices/{id}/voice` | `{"voice":"bf_emma"}` → saved `device_id` and `voice`; scoped links can change only their own cat |
| `POST /api/voice`, `POST /api/text` | Audio or `{"text":"Explain a circle","device_id":"my-echoear"}` → `reply`, `screen`, `session_id`, `audio_url`, timing; voice adds `transcript` |
| `POST /api/intercom` | Audio and `X-Target` (device ID or name) → transcript and delivery result |
| `GET /api/audio/<id>` | Reply PCM from `audio_url`; streams by sentence while synthesis runs |
| `POST /api/tts` | `{"text":"Hello!"}`, optional `device_id` → complete PCM clip using that device's voice |
| `POST /api/reset` | `{"device_id":"my-echoear"}` → forget conversation mapping |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/echoear.bin` with SHA-256 |

Use `X-Device-Id` and `X-Character` to identify the sender. A text or reset body
`device_id` overrides the header. Delivery reports names in `delivered_to` and IDs in `sent_to`/`failed_to`.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, firmware, partition, memory, battery, volume, sleeping, and `ws_connected` |
| `POST /speak` | Raw PCM (1.5 MiB max) → queued sample count or 409 if busy |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull from bridge or upload `echoear.bin` as `application/octet-stream`; reboot on success |
| `POST /reboot` | Reboot |

Legacy HTTP replies still accept optional fenced payloads. `echoear-screen`
becomes `screen`; `echoear-message` routes a message to a peer.

```echoear-screen
{"title":"Radius","shapes":[{"t":"btn","x":85,"y":218,"w":190,"h":46,"s":"Quick fact","tap":"A radius runs from center to edge."}]}
```

Shapes use pixel coordinates and optional `#RRGGBB` colors. Types: `circle`,
`rect`, `line`, `arc`, `text`, `btn`. Use `tap` for local feedback, `ask` for a
bridge follow-up, or `reply_to` on a `btn` for a direct intercom recording.

```echoear-message
{"to":"Mint","text":"Which planet is known for its rings?"}
```

## Networking

Devices and browsers need TCP **8765** to the bridge; direct OTA uses TCP
**80** to the device. Use Avahi (`avahi-resolve -n echoear-XXXX.local`) for
mDNS or the device IP if the container cannot resolve `.local`. HTTP and WS
are unencrypted; use a trusted LAN and keep configs and firmware images private.
