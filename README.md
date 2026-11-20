# EchoEar

EchoEar turns an ESP-VoCat v1.2 into a tap-to-talk voice tutor with interactive diagrams and a cat-to-cat intercom.

```text
EchoEar <-- Wi-Fi PCM, replies, cards --> Python bridge :8765
                                           |-- Hex      (speech to text)
                                           |-- OpenCode (echoear agent)
                                           `-- Kokoro   (local speech)
Bridge :8765 -- intercom card and audio --> Recipient EchoEar :80
```

**Target:** EchoEar / ESP-VoCat v1.2, ESP32-S3, 16 MB flash, 16 MB octal PSRAM,
360×360 touchscreen, and a USB data cable for first flash.

## Quickstart

1. On Linux, install Python 3.11+, Hex 3.4.0, OpenCode V2, Vulkan drivers, Podman,
   `uv`/`uvx`, and `curl`. Use a systemd user session.

   ```sh
   hex model devices
   ./scripts/setup-host.sh --gpu-index 0
   ```

   Pass `--gpu-index 0` only if it selects your intended Vulkan GPU. Setup installs
   Hex's `parakeet_unified_en` and Kokoro models, links the [EchoEar agent](agents/echoear.md),
   and starts both services. Set `ECHOEAR_OPENCODE_MODEL` in `bridge.env` to change models.

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

5. Run `./scripts/check.sh` for offline checks (host GCC required, ShellCheck optional).
   Add `--live` for the bridge suite with Hex, OpenCode, Kokoro, and Flite
   (or pass `--wav` to `bridge/test_bridge.py`).

## Controls and intercom

- Tap the cat or pet its head, then speak after the tone. Voice activity
  detection stops recording at silence; tap or pet again to stop early or interrupt speech.
- Slide across the head pads to change volume, or hold a pad for one step.
  After 120 seconds idle the display dims, unless a diagram is open. Tap or pet
  to wake. The battery pill shows charge.
- **Text** opens the transcript; **New** resets the conversation. **Cat** closes a diagram.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” The bridge bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or **Text → Msg \<Peer\>** to record directly to that cat.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent emits
  an `echoear-message` block; the bridge delivers a card and synthesized speech.

## API

Send `X-EchoEar-Token` except for `/health` and `/status`. Audio is signed 16-bit
little-endian mono PCM at 16 kHz; voice inputs also accept WAV, up to 2 MiB.

| Bridge · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Hex, OpenCode, and TTS readiness |
| `POST /api/register` | `device_id`, `character`, optional `ip` and `port` → other `peers`; only registration can replace an existing address |
| `GET /api/devices` | `devices` with `device_id`, `character`, `ip`, `online` |
| `POST /api/voice`, `POST /api/text` | Audio or `{"text":"Explain a circle","device_id":"my-echoear"}` → `reply`, `screen`, `session_id`, `audio_url`, timing; voice adds `transcript` |
| `POST /api/intercom` | Audio and `X-Target` (device ID or name) → transcript and delivery result |
| `GET /api/audio/<id>` | Reply PCM from `audio_url`; streams by sentence while synthesis runs |
| `POST /api/tts` | `{"text":"Hello!"}` → complete PCM clip |
| `POST /api/reset` | `{"device_id":"my-echoear"}` → forget conversation mapping |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/echoear.bin` with SHA-256 |

Use `X-Device-Id` and `X-Character` to identify the sender. A text or reset body
`device_id` overrides the header. Delivery reports names in `delivered_to` and IDs in `sent_to`/`failed_to`.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, firmware, partition, memory, battery, volume, sleeping, and `peer` (or `null`) |
| `POST /display`, `POST /display/clear` | Raw screen object (8 KiB, 24 shapes max) or no body to clear |
| `POST /speak` | Raw PCM (1.5 MiB max) → queued sample count or 409 if busy |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull from bridge or upload `echoear.bin` as `application/octet-stream`; reboot on success |
| `POST /reboot` | Reboot |

The agent appends optional fenced payloads. `echoear-screen` becomes `screen`;
`echoear-message` routes a message to a peer.

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

Devices need TCP **8765** to the bridge; the bridge and update host need TCP
**80** to the devices. Use Avahi (`avahi-resolve -n echoear-XXXX.local`) for
mDNS. If the container cannot resolve `.local`, run the lookup via `host-spawn`
or use the IP shown on the board. HTTP is unencrypted; use a trusted LAN and
keep local configs and firmware images private.
