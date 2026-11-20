//go:build live

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLiveWebSocket(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	conf := liveConfig(t, home)
	port := conf["ECHOEAR_BRIDGE_PORT"]
	if port == "" {
		port = "8765"
	}
	base := env("ECHOEAR_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port))
	token := conf["ECHOEAR_BRIDGE_TOKEN"]
	client := pooledClient()
	client.Timeout = 115 * time.Second
	defer client.CloseIdleConnections()
	c := liveClient{client, base, token}
	html, res := c.read(t, "/app", nil, "", nil, 200)
	if !bytes.Contains(html, []byte(`<svg id="screen" viewBox="0 0 360 360"`)) ||
		!bytes.Contains(html, []byte("Join as Phone Cat")) ||
		res.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal("missing live app HTML or inline SVG")
	}
	for _, asset := range []string{"/app/style.css", "/app/client.js", "/app/mic.js"} {
		c.read(t, asset, nil, "", nil, 200)
	}
	addresses := func() map[string]string {
		got := liveJSON[struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
				IP       string `json:"ip"`
			}
		}](t, c, "/api/devices", nil, nil, "", nil)
		ips := make(map[string]string, len(got.Devices))
		for _, p := range got.Devices {
			ips[p.DeviceID] = p.IP
		}
		return ips
	}
	before := addresses()

	var suffix [2]byte
	_, _ = rand.Read(suffix[:])
	phoneID := "phone-" + hex.EncodeToString(suffix[:])
	deviceID := "echoear-ws-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	from := testConnectWS(t, base, token, false)
	first := from.hello(t, deviceID, "Cake")
	viewer := testConnectWS(t, base, token, true)
	viewer.json(t, object{"ev": "hello", "role": "web"})
	viewer.until(t, hasWS("peers", deviceID))
	viewer.json(t, object{"ev": "watch", "device_id": deviceID})
	if mirror := viewer.until(t, hasWS("scene", "")); !bytes.Equal(mirror.data, first.data) {
		t.Fatal("live mirror scene differs from the device")
	}
	from.json(t, object{"ev": "status", "battery_pct": 48, "battery_mv": 3840, "charging": false, "volume": 80, "rssi": -43})
	a := from.until(t, hasWS("patch", `"battery-fill"`))
	b := viewer.until(t, hasWS("patch", `"battery-fill"`))
	if !bytes.Equal(a.data, b.data) {
		t.Fatal("live mirror patch differs from the device")
	}

	phone := testConnectWS(t, base, token, true)
	phone.json(t, object{"ev": "hello", "role": "web", "device_id": phoneID, "character": "Plum", "firmware": "web", "volume": 65})
	phone.until(t, hasWS("scene", `"#D8C7FF"`))
	viewer.until(t, hasWS("peers", phoneID))
	pcm := bytes.Repeat([]byte{30, 0}, 320)
	from.json(t, object{"ev": "rec_start", "target": phoneID})
	from.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm...)))
	start := time.Now()
	from.json(t, object{"ev": "rec_end", "cancelled": false})
	phone.until(t, hasWS("chime", `"kind":"intercom"`))
	phone.until(t, hasWS("tts", `"state":"start"`))
	var spoken []byte
	var elapsed time.Duration
	for range 128 {
		f := phone.read(t)
		if f.op == wsBinary {
			if len(f.data) < 3 || f.data[0] != pcmPrefix {
				t.Fatal("bad live intercom frame")
			}
			spoken = append(spoken, f.data[1:]...)
			elapsed = time.Since(start)
		} else if hasWS("tts", `"state":"end"`)(f) {
			break
		}
	}
	if !bytes.HasSuffix(spoken, pcm) {
		t.Fatal("live intercom changed capture PCM")
	}
	phone.json(t, object{"ev": "tts_ack", "state": "done"})
	t.Logf("live raw intercom rec_end to PCM: %.2f ms; HTML/SVG and exact scene/patch mirror verified",
		float64(elapsed)/float64(time.Millisecond))
	for _, s := range []*testWS{from, phone, viewer} {
		s.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
		s.until(t, func(f wsFrame) bool { return f.op == wsClose })
	}
	if len(spoken) == 0 {
		t.Fatal("no live intercom payload")
	}
	after := addresses()
	for id, ip := range before {
		if after[id] != ip {
			t.Fatal("WebSocket test changed a previously registered peer address")
		}
	}
	t.Logf("%d previously registered peer addresses unchanged", len(before))
}
