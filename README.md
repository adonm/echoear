# EchoEar

EchoEar turns an ESP-VoCat v1.2 into a tap-to-talk voice tutor. Custom ESP-IDF v5.5 firmware shows the transcript, answer, and optional LVGL vector cards on the round 360×360 touchscreen. After the first USB flash, update it over Wi-Fi.

```text
EchoEar v1.2 (mic / LVGL touchscreen)
    | PCM over Wi-Fi                     ^
    v                                     | transcript + reply + optional diagram
Python LAN bridge :8765 ------------------+
    |-- audio ------> Anomaly Hex 3.4.0  (parakeet_unified_en, Vulkan GPU STT)
    `-- transcript -> OpenCode V2        (echoear: opencode/gemini-4-neon#high)
```

**Hardware:** EchoEar / ESP-VoCat v1.2 with ESP32-S3, 16 MB flash, 16 MB octal PSRAM, and a USB data cable for the first flash.

## Quickstart

1. **Set up the host.** Use Linux with a systemd user session, Python 3.11+, Hex 3.4.0, OpenCode V2 with access to the configured model, Vulkan drivers, Podman, `uvx`, and `curl`.

   ```sh
   hex model devices
   ./scripts/setup-host.sh --gpu-index 0
   ```

   Use `--gpu-index 0` only if index 0 is your intended Vulkan GPU; omit it to keep Hex's device selection. Setup configures Hex for GPU transcription, installs and selects `parakeet_unified_en`, installs the Vulkan drop-in when requested, symlinks `agents/echoear.md` into OpenCode's user agents, and starts `hex.service` and `echoear-bridge.service`.

2. **Configure firmware.** Create the local config once:

   ```sh
   cp firmware/sdkconfig.local.example firmware/sdkconfig.local
   chmod 600 firmware/sdkconfig.local
   ```

   Set your Wi-Fi SSID and password, the bridge's LAN URL (for example, `http://192.168.1.100:8765`, without a trailing slash), and `ECHOEAR_BRIDGE_TOKEN` from `~/.config/echoear/bridge.env` (under `$XDG_CONFIG_HOME` if set).

3. **Build and flash over USB.**

   ```sh
   ./scripts/build.sh && ./scripts/flash-usb.sh
   ```

   Pass a port such as `/dev/ttyACM0` to flash just one device; otherwise the script flashes every `/dev/ttyACM*` it finds. The screen and `./scripts/monitor.sh` show the device's IP. Tap **Tap to Talk**, speak after the tone, and tap **Tap to Send** (or wait for the 15-second limit). Tap **Text** to leave a diagram, or **New Chat** to start over.

4. **Update over Wi-Fi.** Run `./scripts/push-ota.sh <device-ip>` (replace `<device-ip>` with the device address). It builds, uploads to `/ota/upload`, and checks that the device boots from the other OTA partition. Add `--no-build` to upload the existing image.

**Network:** The EchoEar needs TCP access to the bridge host on port **8765**. Your PC needs TCP access to the EchoEar on port **80** for OTA and direct screen pushes. If IoT Wi-Fi is on another VLAN, allow those connections through the router or put both on the same LAN. Use a trusted LAN: HTTP is unencrypted, and firmware images embed local credentials. Never publish the images, `sdkconfig.local`, or `bridge.env`.

## API reference

Except for `GET /health` and `GET /status`, send the shared `X-EchoEar-Token` request header. Quote token values in local config and do not include actual tokens in shared examples.

### Bridge — port 8765

| Method and path | Request and response |
| --- | --- |
| `GET /health` | JSON with `ok`, `hex`, and `opencode` status flags. |
| `POST /api/voice` | Raw signed 16-bit little-endian mono PCM at 16 kHz (`audio/pcm`) or WAV (`audio/wav`), up to 2 MiB. Set `X-Device-Id` to identify the conversation. Returns `transcript`, `reply`, `screen` (object or `null`), `session_id`, `hex_ms`, and `opencode_ms`. |
| `POST /api/text` | JSON `{"text":"Explain a circle","device_id":"my-echoear"}`. Returns `reply`, `screen`, `session_id`, and `opencode_ms`. |
| `POST /api/reset` | JSON `{"device_id":"my-echoear"}`. Clears the device-to-session mapping and returns `{"ok":true}`. |
| `GET /ota/version.json` | Build manifest with `exists`, byte `size`, `sha256`, and `mtime`. |
| `GET /ota/latest.bin` | Streams local `firmware/build/echoear.bin` and sends `X-Firmware-Sha256`. Returns 404 if there is no image. |

For text and reset, `device_id` takes precedence over `X-Device-Id`. If neither is supplied, the bridge uses `echoear`.

### Device — port 80

| Method and path | Request and response |
| --- | --- |
| `GET /status` | JSON with identity, IP, Wi-Fi signal, free memory, firmware version, and running partition. |
| `POST /display` | Displays a raw screen JSON object (no wrapper), up to 8 KiB and 24 shapes. |
| `POST /display/clear` | Clears the diagram and restores the chat. No body required. |
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
    {"t": "text", "x": 202, "y": 140, "s": "r", "c": "#F8FAFC"}
  ]
}
```

Shapes render in array order. Coordinates are pixels from the top-left; circles, arcs, and text are centered on `x,y`, while rectangles start there. Colors are `#RRGGBB`. The other primitives are `rect` (`x,y,w,h`, optional corner radius `r`) and `arc` (`x,y,r,a1,a2`, angles in degrees). Circles and rectangles accept `fill: true`; lines, circles, and arcs accept stroke width `w`.

## Customize and troubleshoot

Edit [agents/echoear.md](agents/echoear.md) to change the `echoear` prompt and diagram guidance. The bridge allows `webfetch` for this agent and denies other tools, including `edit` and `shell`. To choose a different model, set `ECHOEAR_OPENCODE_MODEL` in `bridge.env` and restart the bridge.

Check the host with `systemctl --user status hex.service echoear-bridge.service` and `curl -fsS http://localhost:8765/health`. Run `python3 bridge/test_bridge.py` for the live Hex/OpenCode smoke test. It uses Flite to generate speech, or accepts a spoken test WAV with `--wav`.
