# EchoEar

EchoEar is a tap-to-talk companion with interactive diagrams, voice notes, live calls, and a browser mirror.

```text
EchoEar <-- /ws: RGB565 frames, touch, PCM --> Go bridge :8765 <-- /app: SVG --> Browser
                                              |-- Hex      (speech to text)
                                              |-- OpenCode :49375 (loopback, reply deltas over SSE)
                                              `-- Kokoro   (persistent FP32 worker)
```

**Targets:** ESP-VoCat v1.2 and ESP32-S3-BOX-3. Both use a 360×360 logical canvas,
centered on the BOX-3’s rectangular display. The bridge runs apps and renders
RGB565 updates for hardware and SVG for `/app`. Boards draw a fallback face offline.

**New in 0.9.3:** Both boards use the touchscreen for all touch controls.
VoCat’s capacitive head pads are disabled.

**New in 0.9.2:** Live two-way **Call** and all nine Kokoro voices in the paged
on-device picker. Firmware announces features in `hello` and flushes only the
dirty LCD columns within changed rows. The bridge and dashboard add paginated
chat history, faster face rendering, and separate turn and hardware diagnostics.

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
   and has no battery gauge.

4. Update via `./scripts/push-ota.sh <ip|host>`; add `--no-build` to reuse the
   image, and prefix with `BOARD=box3` for BOX-3. The script refuses a board
   mismatch. Direct OTA works without the bridge and reads the token from the
   environment or `bridge.env`. The board confirms the new image after display
   initialization and Wi-Fi DHCP; the script checks the running partition.

5. Run `./scripts/check.sh` for Go vet, race tests, and host VAD, frame decoder,
   and graphics tests (strict GCC with ASan/UBSan, both display sizes).
   Add `--live` to exercise the running services; ShellCheck runs if installed.

## Controls, voice notes, and calls

- Tap the cat face and speak after the tone. Pause 1.5 seconds to send
  automatically, or 3 seconds once you've spoken for 4 seconds. Tap **Send**
  to finish early, **Cancel** to discard, or **Stop** to interrupt a reply.
- The display dims after 120 seconds idle. Tap the touchscreen to wake it.
- The on-device **Menu** and `/app` live screen share the same touch controls.
  **Games** launches saved apps, **Memories** shows the cat's saved memories,
  and **History** shows the last 20 messages, newest first. Tap a row for details.
  **Talk to…** opens intercom, **Volume** sets the level or enables **Auto**,
  **Voice** pages through all nine voices, and **Stats** opens the diagnostics HUD.
  Auto-volume estimates ambient noise from recordings and adjusts the DAC in RAM
  without writing to flash. **Cat** returns to the face. Scoped tunnels don't
  expose Games, Memories, or Stats.
- Say “Tell Mint dinner is ready,” “Intercom Cake, come play,” or “Tell everyone
  good morning.” The bridge bypasses OpenCode and relays a chime, sender announcement, and normalized voice.
- Tap **Reply to \<Sender\>** on a message card or choose a peer in
  **Menu → Talk to…** to record directly to that cat.
- Tap **Call** on an incoming Intercom card, pick a peer in **Menu → Call**, or
  say “call \<name\>.” Both cats must be online and call-capable. The recipient
  can answer or decline; either side can mute its mic or hang up. Unlike a
  voice note, a call streams both ways in real time.
- Ask for a composed message (“Send Mint a knock-knock joke”). The agent uses
  `emit` to deliver a card and synthesized speech to the recipient.

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
Chat updates live over `/ws`; **Load older** fetches earlier messages across
sessions. Use **Call** from a virtual cat in `/app` to call a compatible board
or another virtual cat. Mirroring a physical cat does not turn the browser into
its microphone. Use **Photo** to show an image on the selected cat's
screen and leave a card in its chat. Use **Voice** under the screen for one-click
Heart or Fable, or choose among Heart and all eight UK voices in the dropdown. The
choice is saved per device in the bridge database. Mint defaults to Fable (`bm_fable`,
UK English). Cake and other cats default to Heart (`af_heart`, US English)
unless `ECHOEAR_TTS_VOICE` sets a different fallback.
The conversation panel shows active timers for the selected cat with a
countdown and a Cancel button. The LAN sidebar's **Saved Memory & Assets**
section lets you inspect and delete saved keys and visuals.

Plain LAN HTTP supports the SVG mirror, text chat, and audio playback.
Browser calls and recording need microphone access (HTTPS or localhost).
Voice notes use the same adaptive silence timing as the boards. On manual stop, the browser flushes any buffered
audio before ending the recording, without a fixed recording tail. For a remote
phone, install the [OpenTunnel CLI](https://opentunnel.xyz) on the bridge host. If it is
outside the bridge service's `PATH` and `~/.local/bin`, set
`ECHOEAR_OPENTUNNEL_BIN` to its path in `bridge.env`. Restart the bridge
with `systemctl --user restart echoear-bridge.service` so it finds the CLI.
In the dashboard, select a Virtual Cat, choose **Create Tunnel** to start
its route, then **Copy Share Link**. The HTTPS link opens the same cat and
conversation as the LAN dashboard, with mic, screen, voice notes, and calls to
online EchoEars. The per-cat token does not open the LAN dashboard, admin routes, or
other cats' chats. Treat the link as a password and use **Close Tunnel** to revoke it.

### Geeky diagnostics

Open **Menu → Stats** for a compact on-device HUD, or **Diagnostics** in the
LAN `/app` dashboard for device status, pipeline timings, and CPU and memory
usage for the bridge, Kokoro, and host. Firmware reports ESP32-S3 CPU
usage, free and minimum free heap, free PSRAM, Wi-Fi RSSI, and uptime every five
seconds over WebSocket. The dashboard also shows WebSocket round-trip time.
Device status also reports the last frame revision and LCD flush duration,
input overflows, and PCM starvation/drop counts. Turn diagnostics separate
OpenCode gate wait and first sentence, TTS queue and worker time, first frame
render, and elapsed time to the first queued PCM (or HTTP audio URL).

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
After a change to MCP schemas, instructions, the agent prompt, or the runtime
contract version, the bridge reconnects EchoEar MCP and compacts active
OpenCode sessions on startup without changing canonical chat history.
Code Mode exposes exactly four MCP tools under `tools.echoear`:

| Tool | Purpose |
| --- | --- |
| `state` | Read keyed devices, cats, saved apps, timers, and scoped JSON. Select slices with `include`; add `saved_cats`, `messages`, or `diagnostics` when needed. |
| `patch` | Update device settings, cat appearance, memory, or a timer. Memory batches commit atomically; `expected_version:0` requires an absent key. |
| `app` | Show SVG, run or update a JS app, save/load/delete by slug, or exit to the face. |
| `emit` | Send spoken intercom and a card, play notes, or play a named sound (`sfx` or `chime`). |

With `device_id`, `state` always returns `me.device`, `me.cat`, and
`me.active_app`. Read source from `me.cat.face_svg` or fetch saved app source
with `include_code:true`. Read persistent values as `memory[scope][key].value`.

The agent can also use OpenCode's `websearch`. There is no `wait_event` tool:
pointer events and bounded ticks run in the bridge without keeping an OpenCode turn open.
The bridge uses the pure-Go `goja` sandbox to run JS that renders SVG. Apps can
target one player or share state across multiple devices with a separate SVG
view for each player. `initial_state` seeds `ctx.initial_state` (or the state
directly if there is no `init`). `state_patch` shallow-merges live state and
redraws all players without restarting. App state must fit in 64 KB of JSON.
`save_as` writes apps and drawings to Turso's `apps` table; `load` reopens
them. `storeGet(key)` reads persisted state in `init(ctx)`
and `render(state, player)`; `storeSet(key, value)` is available only in event
hooks. Use `onDrag` and `onUp` alongside `onTap` for sliders and drawing in
`/app` or on boards with firmware `0.9.1` or newer. `sfx(name)`, `sfxAll(name)`,
and `emit.sfx` support `boing`, `pop`, `win`, `buzz`, `ding`, and `whoosh`.
In JS, `sfx` and `sfxAll` also accept chimes such as `tap`; unknown names are
silent. App launches return `layout_warnings` for controls that overlap the
Cat exit button or clip on round displays.
Use `app = ui.pagedText({title, text})` for wrapped reading with built-in
navigation, or return `ui.choices({title, prompt, choices})` from `render`
for a card or quiz with up to four buttons. Both leave room for Cat and
fit the round screen. `ui.wrap` and `ui.esc` are available for custom SVG.
Set `timer.app` to a saved app name to launch it on the connected device when
the reminder goes off. See the [agent guide](agents/echoear.md) for examples.

**One Voice Rule:** Only assistant prose is spoken as the reply, not tool
output or reasoning. The bridge checks the streamed speech against the final
reply. The prompt starts with a short identity and source header, plus setup
or personality hints when applicable. The agent uses `state` for live context.

## Bridge data

The bridge uses the embedded Turso Go driver without CGO. Persistent state lives
in `~/.local/share/echoear/echoear.db` (or `$XDG_DATA_HOME/echoear/echoear.db`);
`ECHOEAR_DB_PATH` overrides it. Keep the data directory private (0700, database
0600). The first boot imports valid rows from the six legacy JSON files in that
directory in one transaction and never modifies those backups.
The bridge also writes private daily snapshots to `backups/echoear-YYYY-MM-DD.db`
under its data directory, keeps the seven latest, and reports the latest in
**Diagnostics**.
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
Hardware needs firmware `0.8.0` or newer and announces `proto: 2` in its
authenticated `/ws` `hello`. Firmware `0.9.2` and newer report `caps` for battery,
head pads, Call, and drag. Firmware `0.9.3` reports `head_pads: false` on both
boards. The bridge remembers each board there, using the socket IP and port 80
unless the hello specifies a port. The SVG renderer
streams dirty rectangles as `0x03` RGB565 binary frames, using RLE565 when
smaller. BOX-3 announces its native size; the bridge centers the square
artwork and maps raw touch coordinates back into SVG space. Browsers receive
`op: "svg"` with `device_id`; controls share the boards’ hit targets and also
work with Tab, Enter, and Space.
`0x01` mic upload and `0x02` playback carry 16 kHz signed 16-bit
little-endian mono PCM. Voice capture and intercom use WebSocket. The bridge
pads voiced PCM sent to Hex with 150 ms of silence on each side. Kokoro starts
synthesizing sentences while OpenCode streams its reply over SSE. Firmware
`0.9.0` removes the legacy `/speak` endpoint.

| Bridge · port 8765 | Request → response |
| --- | --- |
| `GET /health` | Hex, OpenCode, and TTS readiness |
| `GET /ws`, `GET /app` | WebSocket hub and embedded browser UI |
| `GET /api/diagnostics` | LAN-only device telemetry, WebSocket RTT, pipeline timings, and bridge, Kokoro, and host resource usage |
| `GET /api/devices` | Remembered devices with `device_id`, `character`, `cat`, `voice`, and current `ws_connected`; hardware also includes `ip`, `port`, and `last_seen` |
| `GET /api/scope` | Current scope and ordered `voices` catalog |
| `POST /api/virtual-clients` | `{}` creates a blank cat; optional legacy `character` and `theme` seed a designed cat |
| `GET /api/cats`, `DELETE /api/cats/{slug}` | List or delete LAN-only saved cat profiles |
| `POST /api/cats/reassign` | LAN-only `from_device_id`, `to_device_id`, and `mode` (`swap` or `move`) → transfer active cats, voices, and sessions |
| `POST /api/devices/{id}/cat` | Update active cat fields or send `{"slug":"cake"}` to load a profile (LAN-only) |
| `GET /api/devices/{id}/messages`, `GET /api/history?device_id={id}` | Canonical chat, newest page first and chronological within each page. `limit` defaults to 100 (max 500). If `has_more`, pass `next_cursor.before_ts` and `next_cursor.before_id` as query parameters to load older messages. |
| `POST /api/devices/{id}/photo`, `GET /api/photos/{id}` | Share a PNG, JPEG, or WebP up to 2 MiB, or fetch the stored square PNG |
| `POST /api/devices/{id}/voice` | `{"voice":"af_heart"}` → saved `device_id` and `voice`; scoped links can change only their own cat |
| `POST /api/chat` | `{"text":"Explain a circle","device_id":"my-echoear"}` → accepted on a connected device; otherwise returns `reply`, `session_id`, and `audio_url` (LAN only) |
| `GET /api/audio/<id>` | Reply PCM from `audio_url`; streams by sentence while synthesis runs |
| `POST /api/tts` | `{"text":"Hello!"}`, optional `device_id` → complete PCM clip using that device's voice |
| `POST /api/reset` | `{"device_id":"my-echoear"}` → save the designed cat, restore the blank starter, and start a fresh session |
| `POST /api/opencode/compact` | LAN-only; optional `device_id` selects one session (default: all), optional `reconnect_mcp: true` refreshes MCP first → `ok`, `compacted`, and `skipped` device IDs |
| `GET /api/memory?scope=…`, `GET /api/memory/scopes`, `DELETE /api/memory?scope=…&key=…` | LAN-only saved JSON and populated scopes |
| `GET /api/apps`, `DELETE /api/apps/{name}`, `POST /api/apps/run` | List, delete, or run saved apps and drawings (LAN only) |
| `GET /api/timers?device_id=…`, `DELETE /api/timers/{id}` | List/cancel active timers; scoped links can see and cancel only their own cat's timers |
| `GET /ota/version.json`, `GET /ota/latest.bin` | Build manifest or `firmware/build/echoear.bin` with SHA-256 |
| `GET /ota/box3/version.json`, `GET /ota/box3/latest.bin` | BOX-3 manifest or `firmware/build-box3/echoear_box3.bin` with SHA-256 |

Use the exact `device_id` in chat requests. To send an intercom without an
agent turn, say or type “Tell Mint dinner is ready,” substituting a nearby
cat’s name. Intercom requires a connected recipient.

| Device · port 80 | Request → response |
| --- | --- |
| `GET /status` | Identity, board, display size, firmware, partition, CPU, memory, RSSI, uptime, battery, volume, sleeping, and `ws_connected` |
| `POST /volume` | `{"level":65}` → saved volume (0–100) |
| `POST /ota`, `POST /ota/upload` | Pull or upload the board's firmware (`application/octet-stream`); reboot on success |
| `POST /reboot` | Reboot |

## Networking

Devices and browsers need TCP **8765** to the bridge; direct OTA uses TCP
**80** to the device. Use Avahi (`avahi-resolve -n echoear-XXXX.local`) for
mDNS or the device IP if the container cannot resolve `.local`. HTTP and WS
are unencrypted; use a trusted LAN and keep configs and firmware images private.
