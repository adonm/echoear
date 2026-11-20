package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type testWS struct {
	conn net.Conn
	in   *bufio.Reader
}

const testKey = "dGhlIHNhbXBsZSBub25jZQ=="
const testAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="

func dialTestWS(t *testing.T, base, token string, query bool, headers object) (*testWS, *http.Response) {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("invalid WebSocket test address")
	}
	u.Path = "/ws"
	if query {
		u.RawQuery = url.Values{"token": {token}}.Encode()
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatal("cannot connect to WebSocket test address")
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	req := &http.Request{Method: "GET", URL: u, Host: u.Host, Header: http.Header{}}
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", testKey)
	if !query && token != "" {
		req.Header.Set("X-EchoEar-Token", token)
	}
	for k, v := range headers {
		req.Header.Set(k, str(v))
	}
	if req.Write(conn) != nil {
		t.Fatal("cannot write upgrade request")
	}
	in := bufio.NewReader(conn)
	res, err := http.ReadResponse(in, req)
	if err != nil {
		t.Fatal("cannot read upgrade response")
	}
	return &testWS{conn, in}, res
}

func testConnectWS(t *testing.T, base, token string, query bool) *testWS {
	t.Helper()
	ws, res := dialTestWS(t, base, token, query, nil)
	if res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != testAccept {
		t.Fatalf("upgrade failed: HTTP %d", res.StatusCode)
	}
	return ws
}

func maskedFrame(op byte, final bool, data []byte) []byte {
	if final {
		op |= 128
	}
	f := []byte{op}
	switch n := len(data); {
	case n < 126:
		f = append(f, 128|byte(n))
	case n <= 65535:
		f = binary.BigEndian.AppendUint16(append(f, 128|126), uint16(n))
	default:
		f = binary.BigEndian.AppendUint64(append(f, 128|127), uint64(n))
	}
	mask := []byte{0xb1, 0xf1, 0x27, 0xe6}
	f = append(f, mask...)
	for i, c := range data {
		f = append(f, c^mask[i%4])
	}
	return f
}

func (s *testWS) write(t *testing.T, data []byte) {
	t.Helper()
	_ = s.conn.SetWriteDeadline(time.Now().Add(4 * time.Second))
	if _, err := s.conn.Write(data); err != nil {
		t.Fatal("cannot send WebSocket frame:", err)
	}
}

func (s *testWS) json(t *testing.T, v object) {
	t.Helper()
	s.write(t, maskedFrame(wsText, true, textFrame(v).data))
}

func (s *testWS) read(t *testing.T) wsFrame {
	t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var h [8]byte
	if _, err := io.ReadFull(s.in, h[:2]); err != nil {
		t.Fatal("cannot read WebSocket frame:", err)
	}
	if h[0]&0xf0 != 0x80 || h[1]&128 != 0 {
		t.Fatal("invalid server frame header")
	}
	op, n := h[0]&15, uint64(h[1])
	if n == 126 {
		if _, err := io.ReadFull(s.in, h[:2]); err != nil {
			t.Fatal(err)
		}
		n = uint64(binary.BigEndian.Uint16(h[:2]))
	} else if n == 127 {
		if _, err := io.ReadFull(s.in, h[:]); err != nil {
			t.Fatal(err)
		}
		n = binary.BigEndian.Uint64(h[:])
	}
	if n > maxPCM+1 {
		t.Fatal("oversized server frame")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(s.in, data); err != nil {
		t.Fatal(err)
	}
	return wsFrame{op, data}
}

func (s *testWS) until(t *testing.T, match func(wsFrame) bool) wsFrame {
	t.Helper()
	for range 256 {
		f := s.read(t)
		if match(f) {
			return f
		}
		if f.op == wsPing {
			s.write(t, maskedFrame(wsPong, true, f.data))
		} else if f.op == wsClose {
			t.Fatal("unexpected close frame")
		}
	}
	t.Fatal("wanted operation not found in WebSocket frames")
	return wsFrame{}
}

func hasWS(op, sample string) func(wsFrame) bool {
	return func(f wsFrame) bool {
		return f.op == wsText && bytes.Contains(f.data, []byte(`"op":"`+op+`"`)) && bytes.Contains(f.data, []byte(sample))
	}
}

func (s *testWS) hello(t *testing.T, id, character string) wsFrame {
	t.Helper()
	s.json(t, object{"ev": "hello", "role": "device", "device_id": id, "character": character, "firmware": "0.7.0", "battery_pct": 100, "volume": 80})
	return s.until(t, hasWS("scene", ""))
}

func wsHarness(t *testing.T, setup func(*bridge, string)) (*bridge, string) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	db := testDB(t, filepath.Join(dir, "echoear.db"), "")
	b := &bridge{root: ctx, token: "unit", db: db, peers: peers{deviceStore(db)}, peerHTTP: pooledClient()}
	if setup != nil {
		setup(b, dir)
	}
	b.ws = newWSHub(b)
	srv := httptest.NewServer(b)
	t.Cleanup(func() {
		b.ws.shutdown()
		cancel()
		srv.Close()
	})
	return b, srv.URL
}

func TestLiveTurnContext(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const a, other = "echoear-context", "echoear-from"
	c, sender := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", false)
	c.hello(t, a, "Cake")
	sender.hello(t, other, "Mint")
	toolTest(t, base, "timer", object{"device_id": a, "op": "set", "in_seconds": 60}, false)
	toolTest(t, base, "scene", object{"device_id": a, "title": "Stars ]", "room": "sky"}, false)
	ctx := turnContext{at: time.Now()}
	b.ws.turnScreen(a, &ctx)
	ctx.timers = b.timers.activeCount(t.Context(), a)
	if got := turnPrompt(peer{DeviceID: a}, false, "tap", "pick", legacyCat("Cake", "cake"), ctx); ctx.timers != 1 ||
		!strings.Contains(got, `, screen=scene "Stars ]" rev=`) || !strings.HasSuffix(got, ", room=sky, timers=1]\npick") ||
		stripEchoEarHeader(got) != "pick" {
		t.Fatal(got)
	}
	toolTest(t, base, "intercom", object{"from_device_id": other, "to_device_id": a, "text": "Hi"}, false)
	toolTest(t, base, "scene", object{"device_id": a, "preset": "face"}, false)
	ctx = turnContext{at: time.Now()}
	b.ws.turnScreen(a, &ctx)
	if got := turnPrompt(peer{DeviceID: a}, false, "typed", "hi", blankCat(), ctx); !strings.HasSuffix(got, ", screen=face, last_intercom=Mint, setup]\nhi") ||
		stripEchoEarHeader(got) != "hi" {
		t.Fatal(got)
	}
	ctx = turnContext{at: ctx.at.Add(10*time.Minute + time.Second)}
	b.ws.turnScreen(a, &ctx)
	if ctx.lastIntercom != "" {
		t.Fatal("expired intercom appeared in live context")
	}
}

func TestWSHandshakeAndRFC6455(t *testing.T) {
	b, base := wsHarness(t, nil)
	for _, c := range []struct {
		token string
		query bool
		h     object
		want  int
	}{
		{"", false, nil, 401},
		{"wrong", true, nil, 401},
		{"unit", true, object{"Origin": "http://example.test"}, 403},
		{"unit", true, object{"Sec-WebSocket-Version": "12"}, 426},
		{"unit", false, object{"Sec-WebSocket-Key": "dGhpcw=="}, 400},
	} {
		_, res := dialTestWS(t, base, c.token, c.query, c.h)
		if res.StatusCode != c.want || c.want == 426 && res.Header.Get("Sec-WebSocket-Version") != "13" {
			t.Fatalf("handshake HTTP %d, wanted %d", res.StatusCode, c.want)
		}
		res.Body.Close()
	}
	c := testConnectWS(t, base, "unit", false)
	c.write(t, slices.Concat(maskedFrame(wsText, false, []byte(`{"ev":"hel`)),
		maskedFrame(wsPing, true, []byte("hi")), maskedFrame(0, true, []byte(`lo","role":"web"}`))))
	pong := c.read(t)
	if pong.op != wsPong || string(pong.data) != "hi" {
		t.Fatal("ping was not echoed during fragmentation")
	}
	c.until(t, hasWS("peers", `"devices":[]`))
	reply := wsCloseFrame(1000)
	c.write(t, maskedFrame(wsClose, true, reply.data))
	if got := c.read(t); got.op != wsClose || !bytes.Equal(got.data, reply.data) {
		t.Fatal("close not echoed")
	}
	for _, tt := range []struct {
		name string
		raw  []byte
		code uint16
	}{
		{"unmasked", []byte{0x81, 0}, 1002},
		{"reserved bit", []byte{0xc1, 0x80}, 1002},
		{"orphan continuation", maskedFrame(0, true, nil), 1002},
		{"fragmented ping", maskedFrame(wsPing, false, []byte("hi")), 1002},
		{"nonminimal length", []byte{0x81, 0xfe, 0, 1}, 1002},
		{"invalid UTF-8", maskedFrame(wsText, true, []byte{0xff}), 1007},
		{"invalid close code", maskedFrame(wsClose, true, []byte{3, 237}), 1002},
		{"oversize", maskedFrame(wsBinary, true, make([]byte, wsLimit+1)), 1009},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testConnectWS(t, base, "unit", true)
			c.write(t, tt.raw)
			got := c.read(t)
			if got.op != wsClose || len(got.data) != 2 || binary.BigEndian.Uint16(got.data) != tt.code {
				t.Fatalf("bad close frame: %+v", got)
			}
		})
	}
	shutdown := testConnectWS(t, base, "unit", true)
	b.ws.shutdown()
	if got := shutdown.read(t); got.op != wsClose || !bytes.Equal(got.data, wsCloseFrame(1001).data) {
		t.Fatalf("hub did not send a going-away close: %+v", got)
	}
}

func nodeIn(scene sceneView, id string) object {
	for _, n := range scene.nodes {
		if n["id"] == id {
			return n
		}
	}
	return nil
}

func TestWSKeyedCatScenes(t *testing.T) {
	now := time.Unix(5000, 0)
	poses := []catPose{
		poseFrame(now, now, now, "idle", 100, 80, true, false),
		poseFrame(now.Add(3400*time.Millisecond), now, now, "idle", 100, 80, true, false),
		{mood: "listening", battery: 100, charge: true},
		{mood: "thinking", dot: 2, battery: 15},
		{mood: "speaking", mouth: true, battery: 15, meter: true, volume: 72},
		{mood: "sleepy", battery: 15},
	}
	for _, char := range []string{"Cake", "Mint", "Plum", "Sunny"} {
		cat := legacyCat(char, char)
		s := catScene(cat, poses[0])
		if len(s.nodes) > 48 || s.bg != cat.FaceBG || nodeIn(s, "stripe-mid")["c"] != cat.MarkColor {
			t.Fatalf("wrong character geometry or palette for %s", char)
		}
		ids := map[string]bool{}
		for _, n := range s.nodes {
			id := str(n["id"])
			if id == "" || len(id) > 24 || ids[id] || color(n["c"]) == "" || strings.Contains(id, "cheek") || strings.Contains(id, "ear-") {
				t.Fatalf("invalid keyed node %v", n)
			}
			ids[id] = true
		}
		if v := sceneDiff(s, s, 2); v != nil {
			t.Fatal("identical scene generated a patch")
		}
		for i, pose := range poses[1:] {
			next := catScene(cat, pose)
			diff := sceneDiff(s, next, uint64(i+2))
			if diff["op"] != "patch" || len(diff["add"].([]object))+len(diff["del"].([]string)) != 0 {
				t.Fatalf("pose should patch same keys: %v", diff)
			}
			set := diff["set"].(map[string]object)
			for j, old := range s.nodes {
				updated := maps.Clone(old)
				maps.Copy(updated, set[str(old["id"])])
				if !reflect.DeepEqual(updated, next.nodes[j]) {
					t.Fatalf("partial fields don't recreate %s", old["id"])
				}
			}
		}
		blink := sceneDiff(s, catScene(cat, poses[1]), 2)["set"].(map[string]object)
		if len(blink) != 4 || blink["eye-l"]["vis"] != false || blink["lid-r"]["vis"] != true {
			t.Fatal("blink should just toggle eyes/lids")
		}
		listening, thinking, speech, sleep := catScene(cat, poses[2]), catScene(cat, poses[3]), catScene(cat, poses[4]), catScene(cat, poses[5])
		if nodeIn(listening, "cat-ring")["c"] != "#FF5964" || nodeIn(listening, "eye-l")["w"] != 40 ||
			nodeIn(thinking, "lid-r")["vis"] != true || nodeIn(thinking, "dot-2")["y"] != 247 ||
			nodeIn(speech, "tongue")["vis"] != true || nodeIn(speech, "volume-track")["on"] != "drag" ||
			nodeIn(sleep, "eye-sleep-l")["vis"] != true || nodeIn(sleep, "doze-large")["vis"] != true {
			t.Fatalf("incorrect mood details for %s", char)
		}
	}
}

func TestWSDiagramAndIntercomConversion(t *testing.T) {
	payload := sanitizeScreen(`{"title":"Moon","shapes":[
		{"t":"circle","x":180,"y":130,"r":25,"w":5,"c":"#ffdd55","tap":"Look up."},
		{"t":"rect","x":0,"y":0,"w":35,"h":25,"c":"#001122","fill":true},
		{"t":"btn","x":110,"y":213,"w":140,"h":37,"c":"#aabbcc","s":"Explore","tap":"Orbit!","ask":"Why does it orbit?"},
		{"t":"btn","x":128,"y":260,"w":104,"h":31,"c":"#1177aa","s":"Reply","reply_to":"echoear-33d8"},
		{"t":"arc","x":180,"y":180,"r":52,"a1":0,"a2":360,"w":9,"c":"#ff5555"}]}`)
	s := screenScene(payload, "#FACC15")
	check := func(id, kind, value string) {
		t.Helper()
		if _, a := s.action(id, 180, 180, "tap"); a != (sceneAction{kind, value}) {
			t.Fatalf("wrong action %q: %+v", id, a)
		}
	}
	check("shape-0", "tap", "Look up.")
	check("shape-2", "ask", "Why does it orbit?")
	check("shape-3", "reply_to", "echoear-33d8")
	if _, a := s.action("", 180, 231, "tap"); a.kind != "ask" {
		t.Fatal("coordinate hit did not pick the button")
	}
	if from := nodeIn(s, "shape-0"); from["sw"] != 5 || from["w"] != nil {
		t.Fatal("circle stroke is not sw")
	}
	if nodeIn(s, "shape-1")["w"] != 35 || nodeIn(s, "shape-1")["x"] != 0 ||
		nodeIn(s, "shape-4")["a2"] != 360 || nodeIn(s, "shape-4")["sw"] != 9 {
		t.Fatal("diagram did not preserve rect/arc coordinates")
	}
	hint := s.hint(s.feedback["shape-2"])
	if n := nodeIn(*hint, "subtitle"); n["s"] != "Orbit!" || n["vis"] != true {
		t.Fatal("diagram hint is missing")
	}
	card := intercomScene(peer{DeviceID: "echoear-33d8", Character: "Cake"}, "Tea is ready.", legacyCat("Cake", "cake"))
	if _, a := card.action("reply", 180, 240, "tap"); a.value != "echoear-33d8" {
		t.Fatal("reply routes by character instead of device ID")
	}
}

func TestWSMirrorAndControls(t *testing.T) {
	_, base := wsHarness(t, nil)
	dev := testConnectWS(t, base, "unit", false)
	first := dev.hello(t, "echoear-33d8", "Cake")
	if bytes.Contains(first.data, []byte(`"id":"new"`)) {
		t.Fatal("New is still on the cat face")
	}
	face := catScene(legacyCat("Cake", "cake"), catPose{})
	if text := nodeIn(face, "text"); nodeInt(text, "x")*2+nodeInt(text, "w") != 360 {
		t.Fatal("Text button is not centered")
	}
	web := testConnectWS(t, base, "unit", true)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.until(t, hasWS("peers", `"ws":true`))
	web.json(t, object{"ev": "watch", "device_id": "echoear-33d8"})
	if f := web.until(t, hasWS("scene", "")); !bytes.Equal(first.data, f.data) {
		t.Fatal("watch does not receive the exact device scene")
	}
	dev.json(t, object{"ev": "status", "battery_pct": 25, "battery_mv": 3530, "charging": false, "volume": 80, "rssi": -45})
	pick := hasWS("patch", `"battery-fill"`)
	onDevice, onWeb := dev.until(t, pick), web.until(t, pick)
	if !bytes.Equal(onDevice.data, onWeb.data) {
		t.Fatal("mirror patch differs from device")
	}
	web.json(t, object{"ev": "tap", "x": 180, "y": 300})
	drawer := func(f wsFrame) bool { return f.op == wsText && bytes.Contains(f.data, []byte(`"drawer-title"`)) }
	if a, b := dev.until(t, drawer), web.until(t, drawer); !bytes.Equal(a.data, b.data) ||
		bytes.Contains(a.data, []byte(`"id":"new"`)) || !bytes.Contains(a.data, []byte(`"id":"voice"`)) {
		t.Fatal("drawer did not mirror voice controls without New")
	}
	web.json(t, object{"ev": "head", "action": "vol_down", "volume": 62})
	for _, c := range []*testWS{dev, web} {
		c.until(t, hasWS("volume", `"level":62`))
	}
	web.json(t, object{"ev": "tap", "id": "cat", "x": 180, "y": 102})
	dev.until(t, hasWS("scene", `"cat-ring"`))
	web.json(t, object{"ev": "tap", "id": "", "x": 180, "y": 180})
	dev.until(t, hasWS("rec", `"action":"start"`))
	dev.json(t, object{"ev": "rec_end", "cancelled": true})
	dev.until(t, hasWS("chime", `"kind":"cancel"`))
}

func setupTestHex(t *testing.T, dir string, handle http.HandlerFunc) hexClient {
	t.Helper()
	srv := httptest.NewServer(handle)
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	path, settings := filepath.Join(dir, "hex-api.json"), filepath.Join(dir, "hex-settings.json")
	if err := writeMap(path, object{"port": p, "token": "unit"}); err != nil {
		t.Fatal(err)
	}
	if err := writeMap(settings, object{"transcription": object{"model": "parakeet_unified_en"}}); err != nil {
		t.Fatal(err)
	}
	return hexClient{upstream{"Hex", pooledClient(), &fileCache[endpoint]{path: path, read: hexEndpoint}},
		&fileCache[string]{path: settings, read: hexQuery}}
}

func TestHexTranscriptionSilencePadding(t *testing.T) {
	pcm := []byte{0, 0, 1, 0, 0, 128, 0, 0}
	for _, tc := range []struct {
		name, kind string
		input      []byte
		pcm, pad   int
	}{
		{"voiced", pcmType, pcm, len(pcm), 4800},
		{"silent", pcmType, make([]byte, 320), 320, 0},
		{"empty", pcmType, nil, 0, 0},
		{"wav passthrough", wavType, WrapWAV(pcm), len(pcm), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setupTestHex(t, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
				got, err := io.ReadAll(r.Body)
				wantSize := 44 + tc.pcm + 2*tc.pad
				if err != nil || len(got) != wantSize || r.ContentLength != int64(wantSize) {
					t.Errorf("Hex WAV length: got %d (HTTP %d), want %d (%v)", len(got), r.ContentLength, wantSize, err)
				} else {
					payload := tc.input
					if tc.kind == wavType {
						payload = tc.input[44:]
					}
					pad := make([]byte, tc.pad)
					want := WrapWAV(slices.Concat(pad, payload, pad))
					if !bytes.Equal(got, want) {
						t.Error("Hex WAV header, silence edges, or original PCM changed")
					}
				}
				if r.URL.Query().Get("model") != "parakeet_unified_en" || r.Header.Get("Content-Type") != wavType {
					t.Error("unexpected Hex transcription request")
				}
				_, _ = io.WriteString(w, `{"transcript":"Test"}`)
			})
			if text, err := h.transcribe(t.Context(), tc.input, tc.kind); err != nil || text != "Test" {
				t.Fatalf("transcribe: %q, %v", text, err)
			}
		})
	}
}

func TestWSRawIntercomDoesNotWaitForHex(t *testing.T) {
	allowHex := make(chan struct{})
	_, base := wsHarness(t, func(b *bridge, dir string) {
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-allowHex:
				_, _ = io.WriteString(w, `{"transcript":"Voice caption."}`)
			case <-r.Context().Done():
			}
		})
	})
	closed := false
	defer func() {
		if !closed {
			close(allowHex)
		}
	}()
	from, to := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
	from.hello(t, "echoear-a", "Cake")
	to.hello(t, "echoear-b", "Mint")
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	from.json(t, object{"ev": "rec_start", "target": "echoear-b"})
	from.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm[:100]...)))
	from.write(t, maskedFrame(wsBinary, true, []byte{0x03, 1, 2})) // wrong audio direction is ignored
	from.write(t, maskedFrame(wsBinary, false, append([]byte{micPrefix}, pcm[100:140]...)))
	from.write(t, maskedFrame(0, true, pcm[140:]))
	from.json(t, object{"ev": "rec_end"})
	to.until(t, hasWS("chime", `"kind":"intercom"`))
	to.until(t, hasWS("tts", `"state":"start"`))
	got := to.until(t, func(f wsFrame) bool { return f.op == wsBinary })
	if got.data[0] != pcmPrefix || !bytes.Equal(got.data[1:], pcm) {
		t.Fatal("raw intercom did not preserve concatenated PCM frames")
	}
	to.until(t, hasWS("tts", `"state":"end"`))
	to.json(t, object{"ev": "tts_ack", "state": "done"})
	close(allowHex)
	closed = true
	to.until(t, hasWS("patch", "Voice caption."))
	to.json(t, object{"ev": "tap", "id": "reply", "x": 180, "y": 240})
	to.until(t, hasWS("rec", `"target":"echoear-a"`))
	to.json(t, object{"ev": "rec_end", "cancelled": true})
}

func TestVirtualWSModeFollowsPlaybackAck(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "phone-a123"
	phone := testConnectWS(t, base, "unit", true)
	phone.hello(t, id, "Plum")
	stream := newAudioStream()
	stream.append(bytes.Repeat([]byte{12, 0}, sampleRate))
	stream.finish(nil)
	b.ws.mu.Lock()
	d := b.ws.devices[id]
	b.ws.play(d, d.client.ctx, stream)
	b.ws.mu.Unlock()
	phone.until(t, hasWS("mode", `"mode":"speaking"`))
	frame := phone.until(t, hasWS("tts", `"state":"end"`))
	audio, err := testObject(t, frame.data)["audio"].(json.Number).Int64()
	if err != nil || audio < 1 {
		t.Fatal("missing playback ID", err)
	}
	phone.json(t, object{"ev": "tts_ack", "state": "done", "audio": audio + 1})
	phone.json(t, object{"ev": "status", "volume": 42})
	phone.until(t, hasWS("volume", `"level":42`))
	b.ws.mu.Lock()
	awaiting := d.awaitAck
	b.ws.mu.Unlock()
	if !awaiting {
		t.Fatal("a stale acknowledgment changed the current playback mode")
	}
	phone.json(t, object{"ev": "tts_ack", "state": "done", "audio": audio})
	phone.until(t, hasWS("mode", `"mode":"idle"`))
}

func TestWSVoiceStreamsBeforeOpenCodeCompletes(t *testing.T) {
	release := make(chan struct{})
	closeRelease := syncOnceClose(release)
	defer closeRelease()
	pcm := bytes.Repeat([]byte{9, 0}, 350)
	speech := bytes.Repeat([]byte{2, 3}, 220)
	kick := make(chan struct{})
	const first = "Stars release their own light. "
	const last = "That lights nearby planets."
	const raw = first + last + "\n```echoear-screen\n{\"title\":\"Night sky\",\"shapes\":[{\"t\":\"btn\",\"x\":121,\"y\":212,\"w\":118,\"h\":34,\"c\":\"#ffaa33\",\"s\":\"Fact\",\"tap\":\"Stars are bright.\"}]}\n```"
	_, base := wsHarness(t, func(b *bridge, dir string) {
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			got, _ := io.ReadAll(r.Body)
			pad := make([]byte, 4800)
			if !bytes.Equal(got, WrapWAV(slices.Concat(pad, pcm, pad))) {
				t.Error("Hex did not receive padded, framed PCM")
			}
			_, _ = io.WriteString(w, `{"transcript":"What are stars?"}`)
		})
		b.worker = &synthWorker{requests: make(chan ttsRequest), done: make(chan struct{})}
		b.worker.ready.Store(true)
		go func() {
			defer close(b.worker.done)
			for {
				select {
				case <-b.root.Done():
					return
				case req := <-b.worker.requests:
					req.done <- ttsResult{pcm: speech}
				}
			}
		}()
		oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/event":
				w.Header().Set("Content-Type", "text/event-stream")
				f := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
				f.Flush()
				select {
				case <-kick:
				case <-r.Context().Done():
					return
				}
				sse := func(delta string) {
					j, _ := json.Marshal(object{"type": "session.text.delta", "data": object{"sessionID": "ses-ws", "assistantMessageID": "msg2", "ordinal": 0, "delta": delta}})
					fmt.Fprintf(w, "data: %s\n\n", j)
					f.Flush()
				}
				sse(first)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				sse(strings.TrimPrefix(raw, first))
				_, _ = io.WriteString(w, "data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses-ws\"}}\n\n")
				f.Flush()
				<-r.Context().Done()
			case "/api/session":
				_, _ = io.WriteString(w, `{"data":{"id":"ses-ws"}}`)
			case "/api/session/ses-ws/prompt":
				var prompt struct{ Text string }
				if err := json.NewDecoder(r.Body).Decode(&prompt); err != nil ||
					!strings.HasPrefix(prompt.Text, "[EchoEar: Cake (echoear-a), hardware, voice, time=") ||
					!strings.Contains(prompt.Text, ", screen=face]") || stripEchoEarHeader(prompt.Text) != "What are stars?" {
					t.Errorf("unexpected voice prompt: %q (%v)", prompt.Text, err)
				}
				close(kick)
				_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
			case "/api/session/ses-ws/message":
				_ = json.NewEncoder(w).Encode(object{"data": []object{
					{"id": "msg2", "type": "assistant", "finish": "stop", "content": []object{{"type": "text", "text": raw}}},
					{"id": "msg1", "type": "user"},
				}})
			default:
				w.WriteHeader(404)
			}
		}))
		t.Cleanup(oc.Close)
		path := filepath.Join(dir, "oc-api.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, ocSettings{})
	})
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, "echoear-a", "Cake")
	dev.json(t, object{"ev": "rec_start"})
	dev.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm...)))
	dev.json(t, object{"ev": "rec_end", "cancelled": false})
	dev.until(t, hasWS("tts", `"state":"start"`))
	got := dev.until(t, func(f wsFrame) bool { return f.op == wsBinary })
	if got.data[0] != pcmPrefix || !bytes.Equal(got.data[1:], speech) {
		t.Fatal("did not receive eager Kokoro frame")
	}
	closeRelease()
	got = dev.until(t, func(f wsFrame) bool { return f.op == wsBinary })
	if len(got.data) != 1+sampleRate/5+len(speech) || !bytes.HasSuffix(got.data, speech) {
		t.Fatal("did not receive tail sentence")
	}
	dev.until(t, hasWS("tts", `"state":"end"`))
	dev.json(t, object{"ev": "tts_ack", "state": "done"})
	dev.until(t, func(f wsFrame) bool { return hasWS("scene", "Night sky")(f) || hasWS("patch", "Night sky")(f) })
	dev.json(t, object{"ev": "tap", "id": "shape-0", "x": 180, "y": 226})
	dev.until(t, hasWS("patch", "Stars are bright."))
	dev.until(t, func(f wsFrame) bool { return f.op == wsBinary })
}

func syncOnceClose(ch chan struct{}) func() {
	closed := false
	return func() {
		if !closed {
			close(ch)
			closed = true
		}
	}
}

func TestEmbeddedApp(t *testing.T) {
	b := &bridge{}
	for _, path := range []string{"/", "/app", "/app/style.css", "/app/client.js", "/app/mic.js"} {
		w := httptest.NewRecorder()
		b.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if path == "/" {
			if w.Code != 302 || w.Header().Get("Location") != "/app" {
				t.Fatal("missing app redirect")
			}
		} else if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing app asset or security headers", path)
		} else if path == "/app" && (!strings.Contains(w.Body.String(), `viewBox="0 0 360 360"`) || !strings.Contains(w.Body.String(), "New Chat") ||
			!strings.Contains(w.Body.String(), `Join as Phone Cat`)) {
			t.Fatal("app is missing its SVG or phone controls")
		}
	}
}
