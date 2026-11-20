# EchoEar

EchoEar turns an ESP-VoCat v1.2 into a tap-to-talk tutor with interactive diagrams, intercom, and a browser mirror.

```text
EchoEar <-- /ws: RGB565 frames, taps, PCM --> Go bridge :8765 <-- /app: SVG --> Browser
                                              |-- Hex      (speech to text)
                                              |-- OpenCode :49375 (loopback, reply deltas over SSE)
                                              `-- Kokoro   (persistent FP32 worker)
```

**Targets:** ESP-VoCat v1.2 and ESP32-S3-BOX-3. Firmware `v0.8.4` displays
RGB565 updates from the bridge, reports touch coordinates and hardware telemetry,
and draws a fallback face offline. The bridge runs app logic and renders the
display; `/app` uses native SVG.

## Quickstart

1. On Linux, install Go 1.26, Python 3.11+, Hex 3.4.0, OpenCode V2, Vulkan
   drivers, Podman, `uv`/`uvx`, and `curl`. Use a systemd user session.

   ```sh
   hex model devices
   ./scripts/setup-host.sh --gpu-index 0
   ```

   Pass `--gpu-index 0` only if it selects your intended Vulkan GPU. Setup builds
   the Go bridge, installs Hex's `parakeet_unified_en` and FP32 `kokoro-v1.0.onnx`,
   and starts Hex, the bridge, and a dedicated OpenCode service. The default
   model is `opencode/gemini-4-hydrogen#low`; set `ECHOEAR_OPENCODE_MODEL` in
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
   Both board builds use the ESP-IDF 5.5 container and read the same
   `firmware/sdkconfig.local`. Never commit that file or share the resulting binaries.

3. Build and flash the EchoEar v1.2 (`vocat`, 360×360 round):

   ```sh
   ./scripts/build.sh
   ./scripts/flash-usb.sh /dev/ttyACM0
   ./scripts/monitor.sh /dev/ttyACM0
   ```

   For the Espressif ESP32-S3-BOX-3 (320×240 rectangular, USB power):

   ```sh
   BOARD=box3 ./scripts/build.sh
   BOARD=box3 ./scripts/flash-usb.sh /dev/ttyACM0
   ```

   BOX-3 builds into `firmware/build-box3/` instead of `firmware/build/`.
   It starts with a blank cat at `box3-xxxx.local` (last four MAC digits)
   and has no battery gauge or head-touch controls.

4. Update via `./scripts/push-ota.sh <ip|host>`; add `--no-build` to reuse the
   image, and prefix with `BOARD=box3` for BOX-3. The script refuses a board
   mismatch. Direct OTA works without the bridge and reads the token from the
   environment or `bridge.env`. The board confirms the new image after display
   initialization and Wi-Fi DHCP; the script checks the running partition.

5. Run `./scripts/check.sh` for Go vet, race tests, and host VAD, frame decoder,
   and graphics tests (strict GCC with ASan/UBSan, both display sizes).
   Add `--live` to exercise the running services; ShellCheck runs if installed.

## Controls and intercom

- Tap the cat face and speak after the tone. Pause about 2 seconds to send
  automatically, tap **Send** to finish early, or tap **Cancel** to discard the
  recording. Tap the **Stop** pill to interrupt a reply.
- Head touch only pets, wakes, or interrupts the cat; it never changes volume.
  The display dims after 120 seconds idle.
- The on-device **Menu** has large, high-contrast buttons with icons and labels:
  **Talk to…** opens intercom, **Volume** sets the level or enables **Auto**,
  **Voice** chooses a speaking voice, and **Stats** opens the diagnostics HUD.
  Auto-volume estimates ambient noise from recordings and adjusts the DAC in RAM
  without writing to flash. **Cat** closes a diagram.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” The bridge bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or choose a peer in
  **Menu → Talk to…** to record directly to that cat.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  `act` to deliver a card and synthesized speech to the recipient.

## Browser dashboard

Open `http://<host-lan-ip>:8765/app` on a trusted LAN and enter the bridge
token. Select a physical EchoEar to mirror and control it, or use
**+ New Cat** to create a blank virtual cat. Say hi to design its name, colors,
accessories, voice, and personality in chat. **New Chat** archives the current
design and brings back the blank starter without deleting your chat history.
Open **Reassign / Load Cat** in the LAN sidebar to move or swap a design, voice,
and active conversation between devices. Use **Saved Cats** there to assign a
profile to the selected device or delete the saved copy. Shared tunnel links
can chat but cannot reset or reassign cats.
The Chat panel loads the cat's conversation history across sessions and
updates live over `/ws`. Use **Voice** under the screen for one-click Heart or
Fable, or choose among Heart and all eight UK voices in the dropdown. The
choice is saved per device in the bridge database. Mint defaults to Fable (`bm_fable`,
UK English). Cake and other cats default to Heart (`af_heart`, US English)
unless `ECHOEAR_TTS_VOICE` sets a different fallback.
The conversation panel shows active timers for the selected cat with a
countdown and a Cancel button. The LAN sidebar's **Saved Memory & Assets**
section lets you inspect and delete saved keys and visuals.

Plain LAN HTTP supports the SVG mirror, text chat, and audio playback.
Microphone access requires HTTPS or localhost. On stop, the browser captures
another 200 ms before flushing the final audio. For a remote phone, install
the [OpenTunnel CLI](https://opentunnel.xyz) on the bridge host. If it is
outside the bridge service's `PATH` and `~/.local/bin`, set
`ECHOEAR_OPENTUNNEL_BIN` to its path in `bridge.env`. Restart the bridge
with `systemctl --user restart echoear-bridge.service` so it finds the CLI.
In the dashboard, select a Virtual Cat, choose **Create Tunnel** to start
its route, then **Copy Share Link**. The HTTPS link opens the same cat and
conversation as the LAN dashboard, with mic, screen, and intercom to online
EchoEars. The per-cat token does not open the LAN dashboard, admin routes, or
other cats' chats. Treat the link as a password and use **Close Tunnel** to revoke it.

### Geeky diagnostics

Open **Menu → Stats** for a compact on-device HUD, or **Diagnostics** in the
LAN `/app` dashboard for device status, pipeline timings, and CPU and memory
usage for the bridge, Kokoro, and host. Firmware `0.8.4` reports ESP32-S3 CPU
usage, free and minimum free heap, free PSRAM, Wi-Fi RSSI, and uptime every five
seconds over WebSocket. The dashboard also shows WebSocket round-trip time.

## Dedicated OpenCode

`echoear-opencode.service` listens at `127.0.0.1:49375` with Basic auth.
Setup generates its password in the private `bridge.env` and gives it separate
configuration, session data, cache, state, and workspace under
`~/.local/share/echoear/opencode/` (or `$XDG_DATA_HOME`). It links the EchoEar
agent and legacy `auth.json`, and imports the configured model provider's
current V2 credentials once through OpenCode's CLI. It does not share your
session database or load personal plugins, MCP servers, or coding `AGENTS.md`.
Setup does not edit personal OpenCode configuration. If upgrading, remove the
old `mcp.servers.echoear` entry and EchoEar agent symlink from your personal config.
Without `ECHOEAR_OPENCODE_URL`, the bridge still discovers the shared service
through `service.json`. `ECHOEAR_OPENCODE_DIR` overrides the dedicated workspace.

Click **Pair OpenCode** in the LAN dashboard's Devices sidebar to create a
short-lived, single-use link. The link opens the dedicated server via the
`echoear-opencode-lan.socket` TCP forwarder on port `49376`. It does not pair
with your personal OpenCode or work through cat share links. Keep ports `8765`
and `49376` on a trusted LAN; neither uses TLS. Pair only trusted clients: a
paired client gets access to the dedicated OpenCode server, not just cat chat.
To turn off LAN OpenCode access, stop both forwarder units, disable
`echoear-opencode-lan.socket`, remove `ECHOEAR_OPENCODE_PAIR_PORT` from `bridge.env`,
and restart the bridge.

### EchoEar MCP

OpenCode runs the installed `echoear-bridge mcp` stdio proxy, which forwards
requests to the bridge's authenticated `POST /mcp` on loopback. The proxy
reads the token from its environment or the private `bridge.env`; the
OpenCode config contains only the executable and its argument, no secrets.
On startup after MCP tool schemas or server instructions change, the bridge
reconnects EchoEar MCP and compacts active OpenCode sessions without changing
the canonical chat history.
Code Mode exposes exactly four MCP tools under `tools.echoear`:

| Tool | Purpose |
| --- | --- |
| `inspect` | Discover devices or read a cat's live screen, app, peers, timers, and saved state |
| `run_app` | Show SVG, run a JS micro-app, save or load by slug, or exit to the face |
| `act` | Send intercom, play a chime or melody, change volume or sleep, manage reminders, or edit a cat |
| `store` | Read or write scoped JSON with optional version checks |

The agent can also use OpenCode's `websearch`. There is no `wait_event` tool:
taps and bounded ticks run in the bridge without keeping an OpenCode turn open.
The bridge uses the pure-Go `goja` sandbox to run JS that renders SVG. Apps can
target one player or share state across multiple devices with a separate SVG
view for each player. `save_as` writes apps and drawings to Turso's `apps`
table; `load` reopens them. See the [agent guide](agents/echoear.md) for examples.

**One Voice Rule:** Only final assistant prose is spoken as the reply, not
tool output or intermediate text. The prompt starts with a short identity
and source header, plus setup or personality hints when applicable. The agent
uses `inspect` for live state.

## Bridge data

The bridge uses the embedded Turso Go driver without CGO. Persistent state lives
in `~/.local/share/echoear/echoear.db` (or `$XDG_DATA_HOME/echoear/echoear.db`);
`ECHOEAR_DB_PATH` overrides it. Keep the data directory private (0700, database
0600). The first boot imports valid rows from the six legacy JSON files in that
directory in one transaction and never modifies those backups.
Active cat designs and reusable profiles are stored in `cats` and `saved_cats`.
Both the device and browser use one editable `face_svg` for a cat's appearance.
The bridge renders shared artwork and the matching `data-pose` layer:
`idle`, `blink`, `talk`, `listen`, `think`, or `sleep`.
The canonical `messages` table drives browser history. The v2 migration
backfills it from legacy bridge chat events and OpenCode session history.
New rows contain user text, final cat replies, intercom, and display cards,
not tool traces or prompt headers.

Optional Turso Cloud sync runs every minute when both `ECHOEAR_TURSO_URL`
and `ECHOEAR_TURSO_AUTH_TOKEN` are set. Use a dedicated private remote and one
active bridge per database: the data includes chat events and tunnel credentials,
and the bridge, not the cloud, performs timer side effects. No cloud credentials
or connectivity are required for normal local operation.

## API

HTTP API requests use `X-EchoEar-Token` except `/health`, `/app`, and `/status`.
Firmware `0.8.0` announces `proto: 2` in its `/ws` hello. The pure-Go
SVG renderer streams dirty rectangles as `0x03` RGB565 binary frames, using
RLE565 when smaller than raw pixels. The bridge defaults to a 360×360 round
display for older hellos. BOX-3 announces its native size; the bridge centers
the square artwork on its background and maps raw taps back into SVG space.
The browser receives SVG instead.
`0x01` mic upload and `0x02` playback carry 16 kHz signed 16-bit
little-endian mono PCM. HTTP voice
inputs also accept WAV, up to 2 MiB. The bridge appends 150 ms of silence to
PCM sent to Hex for transcription. Kokoro starts synthesizing sentences while
OpenCode streams its reply over SSE.

| Bridge · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Hex, OpenCode, and TTS readiness |
| `GET /ws`, `GET /app` | WebSocket hub and embedded browser UI |
| `GET /api/diagnostics` | LAN-only device telemetry, WebSocket RTT, pipeline timings, and bridge, Kokoro, and host resource usage |
| `POST /api/register` | `device_id`, `character`, optional `ip` and `port` → other `peers`; only registration can replace an existing address |
| `GET /api/devices` | `devices` with `device_id`, `character`, `cat`, `ip`, `online`, `voice` |
| `POST /api/virtual-clients` | `{}` creates a blank cat; optional legacy `character` and `theme` seed a designed cat |
| `GET /api/cats`, `DELETE /api/cats/{slug}` | List or delete LAN-only saved cat profiles |
| `POST /api/cats/reassign` | LAN-only `from_device_id`, `to_device_id`, and `mode` (`swap` or `move`) → transfer active cats, voices, and sessions |
| `POST /api/devices/{id}/cat` | Update active cat fields or send `{"slug":"cake"}` to load a profile (LAN-only) |
| `GET /api/devices/{id}/messages` | Canonical chronological chat for a device |
| `POST /api/devices/{id}/voice` | `{"voice":"af_heart"}` → saved `device_id` and `voice`; scoped links can change only their own cat |
| `POST /api/voice`, `POST /api/text` | Audio or `{"text":"Explain a circle","device_id":"my-echoear"}` → `reply`, `screen`, `session_id`, `audio_url`, timing; voice adds `transcript` |
| `POST /api/intercom` | Audio and `X-Target` (device ID or name) → transcript and delivery result |
| `GET /api/audio/<id>` | Reply PCM from `audio_url`; streams by sentence while synthesis runs |
| `POST /api/tts` | `{"text":"Hello!"}`, optional `device_id` → complete PCM clip using that device's voice |
| `POST /api/reset` | `{"device_id":"my-echoear"}` → save the designed cat, restore the blank starter, and start a fresh session |
| `POST /api/opencode/compact` | LAN-only; optional `device_id` selects one session (default: all), optional `reconnect_mcp: true` refreshes MCP first → `ok`, `compacted`, and `skipped` device IDs |
| `GET /api/memory?scope=…`, `GET /api/memory/scopes`, `DELETE /api/memory?scope=…&key=…` | LAN-only saved JSON and populated scopes |
| `GET /api/assets`, `DELETE /api/assets/{name}` | LAN-only reusable visual assets |
| `GET /api/apps`, `DELETE /api/apps/{name}` | LAN-only saved apps and drawings |
| `GET /api/timers?device_id=…`, `DELETE /api/timers/{id}` | List/cancel active timers; scoped links can see and cancel only their own cat's timers |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/echoear.bin` with SHA-256 |
| `GET /ota/box3/version.json`, `GET /ota/box3/latest.bin` | BOX-3 manifest or `firmware/build-box3/echoear_box3.bin` with SHA-256 |

Use `X-Device-Id` and `X-Character` to identify the sender. A text or reset body
`device_id` overrides the header. Delivery reports names in `delivered_to` and IDs in `sent_to`/`failed_to`.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `ws_connected` |
| `POST /speak` | Raw PCM (1.5 MiB max) → queued sample count or 409 if busy |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull or upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Networking

Devices and browsers need TCP **8765** to the bridge; direct OTA uses TCP
**80** to the device. Use Avahi (`avahi-resolve -n echoear-XXXX.local`) for
mDNS or the device IP if the container cannot resolve `.local`. HTTP and WS
are unencrypted; use a trusted LAN and keep configs and firmware images private.
