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
and serial log show each device's LAN IP. Tap **Tap to Talk**, wait for the
tone, then speak. Tap **Tap to Send**, or wait for the 15-second recording
limit. **New Chat** resets the conversation for that device.

`GET /status` is public on the LAN. The POST endpoints require the
`X-EchoEar-Token` header. Direct Wi-Fi OTA needs no running bridge:

```sh
./scripts/push-ota.sh 192.168.1.101
./scripts/push-ota.sh --no-build 192.168.1.101
```

The script reads `ECHOEAR_BRIDGE_TOKEN` from the environment or
`~/.config/echoear/bridge.env`. The device confirms a new image after display
initialization and Wi-Fi DHCP. Firmware images contain the local credentials;
keep them private. Device management uses unencrypted HTTP and should only be
exposed on a trusted LAN.

Voice replies with a `screen` object open a diagram above the chat. Tap **Text**
to see the transcript or **Tap to Talk** to start a new turn. For direct display,
`POST /display` accepts the screen object (no wrapper) with up to 24 shapes and
`POST /display/clear` restores the chat. Both require `X-EchoEar-Token`.
