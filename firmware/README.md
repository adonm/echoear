# EchoEar firmware

This ESP-IDF 5.5 project targets the ESP-VoCat v1.2. Build it and provision the
boards over USB from the repository root:

```sh
cp firmware/sdkconfig.local.example firmware/sdkconfig.local
# Set the Wi-Fi credentials, bridge URL, and bridge token in sdkconfig.local.
chmod 600 firmware/sdkconfig.local
./scripts/build.sh
./scripts/flash-usb.sh /dev/ttyACM0
./scripts/monitor.sh /dev/ttyACM0
```

`flash-usb.sh` with no port flashes every `/dev/ttyACM*` it finds. The display
and serial log show each device's LAN IP. Tap the cat or pet its head, wait
for the tone, then speak. Voice activity detection stops recording after you
finish; a thinking chime confirms the question was sent. Tap or pet again to
end recording early or stop playback. Use **Text** for the transcript or
**New** to reset the conversation for that device.

`GET /status` is public on the LAN. The POST endpoints require the
`X-EchoEar-Token` header. Direct Wi-Fi OTA needs no running bridge:

```sh
./scripts/push-ota.sh echoear-XXXX.local
./scripts/push-ota.sh --no-build echoear-XXXX.local
```

Replace `XXXX` with the device suffix, or pass the device's LAN IP if mDNS
does not resolve (including when upgrading from older firmware). The script
reads `ECHOEAR_BRIDGE_TOKEN` from the environment or
`~/.config/echoear/bridge.env`. The device confirms a new image after display
initialization and Wi-Fi DHCP. Firmware images contain the local credentials;
keep them private. Device management uses unencrypted HTTP and should only be
exposed on a trusted LAN.

Voice and text follow-up replies with a `screen` object open a diagram while
the answer plays. Tap **Cat** to go back. A diagram primitive can include
`tap` feedback, an `ask` follow-up, or both; `btn` draws a labeled button.
For direct display,
`POST /display` accepts the screen object (no wrapper) with up to 24 shapes and
`POST /display/clear` restores the cat. Both require `X-EchoEar-Token`.
Authenticated `POST /speak` accepts raw 16 kHz signed 16-bit little-endian
mono PCM (up to 1.5 MiB). It returns the sample count when queued.

The cat sends after a pause in speech, or you can tap to send sooner. A short
head pet wakes a sleeping cat or acts like a screen tap. Slide left to right
to raise the volume, right to left to lower it; holding one pad also takes
one step. Authenticated `POST /volume` accepts `{"level":65}` (0–100). The
volume survives reboot.

`GET /status` includes the current volume, sleep state, and battery readings
(`null` when no gauge responds). Each board advertises its port-80 HTTP
service at `echoear-<MAC suffix>.local`.
