//go:build live

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"image"
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
		!bytes.Contains(html, []byte("+ New Cat")) ||
		!bytes.Contains(html, []byte("Reassign / Load Cat")) ||
		!bytes.Contains(html, []byte("New Chat")) ||
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
	first, rev, _ := decodeDisplay(t, from.helloV2(t, deviceID, "Cake"))
	if first != image.Rect(0, 0, 360, 360) {
		t.Fatalf("live board did not receive full initial frame: %v", first)
	}
	viewer := testConnectWS(t, base, token, true)
	viewer.json(t, object{"ev": "hello", "role": "web"})
	viewer.until(t, hasWS("peers", deviceID))
	viewer.json(t, object{"ev": "watch", "device_id": deviceID})
	if mirror := testObject(t, viewer.until(t, hasWS("svg", "cat-ring")).data); str(mirror["rev"]) == "" {
		// JSON revisions are numbers, not strings.
		if mirror["rev"] == nil || rev == 0 {
			t.Fatal("missing initial display revisions")
		}
	}
	from.json(t, object{"ev": "status", "battery_pct": 48, "battery_mv": 3840, "charging": false, "volume": 80, "rssi": -43})
	battery, _, _ := decodeDisplay(t, from.until(t, isDisplay))
	viewer.until(t, hasWS("svg", "#39D1A0"))
	if battery.Dx() > 50 || battery.Dy() > 25 {
		t.Fatalf("battery update did not use a dirty rectangle: %v", battery)
	}
	from.json(t, object{"ev": "tap", "x": 180, "y": 300})
	drawer := viewer.until(t, hasWS("svg", "Voice:"))
	if bytes.Contains(drawer.data, []byte(`data-id=\"new\"`)) {
		t.Fatal("hardware drawer offered reset")
	}
	from.json(t, object{"ev": "tap", "x": 180, "y": 106})
	viewer.until(t, hasWS("svg", "cat-ring"))
	const code = `let text="Ready";
function render(){return '<svg viewBox="0 0 360 360"><rect data-id="flip" x="140" y="95" width="80" height="50" fill="gold"/><text x="180" y="195" fill="white" text-anchor="middle">'+text+'</text></svg>';}
function onTap(s,p,id){if(id==="flip")text="Live tap";}
function onTick(){if(text==="Live tap")text="Live reset";}`
	result := liveJSON[struct{ Result struct{ IsError bool } }](t, c, "/mcp", object{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": object{"name": "run_app", "arguments": object{"devices": deviceID, "title": "Live flip", "code": code, "tick_ms": 500, "max_ticks": 12}}}, nil, "", nil)
	if result.Result.IsError {
		t.Fatal("live MCP run_app rejected local interactivity")
	}
	viewer.until(t, hasWS("svg", "Ready"))
	from.json(t, object{"ev": "tap", "x": 180, "y": 120})
	viewer.until(t, hasWS("svg", "Live tap"))
	viewer.until(t, hasWS("svg", "Live reset"))
	from.json(t, object{"ev": "tap", "x": 180, "y": 316})
	viewer.until(t, hasWS("svg", "cat-ring"))
	t.Log("live cat drawer, raw tap, JS tick, dirty rectangle, SVG mirror, and exit verified")

	phone := testConnectWS(t, base, token, true)
	phone.json(t, object{"ev": "hello", "role": "web", "device_id": phoneID, "character": "New Cat", "firmware": "web", "volume": 65})
	face := phone.until(t, hasWS("svg", "cat-ring"))
	if !hasWS("svg", "#F3F4F6")(face) || !hasWS("svg", "Say hi to design me!")(face) {
		t.Fatal("live Phone Cat did not start blank")
	}
	viewer.until(t, hasWS("peers", phoneID))
	pcm := bytes.Repeat([]byte{30, 0}, 320)
	from.json(t, object{"ev": "rec_start", "target": phoneID})
	from.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm...)))
	start := time.Now()
	from.json(t, object{"ev": "rec_end", "cancelled": false})
	phone.until(t, hasWS("chime", `"kind":"intercom"`))
	phone.until(t, hasWS("tts", `"state":"start"`))
	phone.until(t, hasWS("mode", `"mode":"speaking"`))
	var spoken []byte
	var audio any
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
			audio = testObject(t, f.data)["audio"]
			break
		}
	}
	if !bytes.HasSuffix(spoken, pcm) {
		t.Fatal("live intercom changed capture PCM")
	}
	if audio == nil {
		t.Fatal("missing live playback ID")
	}
	phone.json(t, object{"ev": "tts_ack", "state": "done", "audio": audio})
	phone.until(t, hasWS("mode", `"mode":"idle"`))
	t.Logf("live raw intercom rec_end to PCM: %.2f ms; HTML/SVG and framebuffer mirror verified",
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
