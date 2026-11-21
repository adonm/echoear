//go:build live

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	imagecolor "image/color"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const e2eVoiceQuestion = "Why is the sky blue? Answer in one short sentence."

func TestVirtualE2E(t *testing.T) {
	switch mode := os.Getenv("ECHOEAR_E2E_MODE"); mode {
	case "stub", "real":
		s := startE2E(t, mode == "real")
		t.Run("IdleWake", func(t *testing.T) {
			t.Parallel()
			c := s.board(t, "esp-vocat", "Idle")
			s.idleWake(t, c, time.Now())
		})
		t.Run("CallTimeout", func(t *testing.T) {
			t.Parallel()
			s.callTimeout(t)
		})
		t.Run("ParentBudget", func(t *testing.T) {
			t.Parallel()
			s.parentBudget(t)
		})
		t.Run("Product", func(t *testing.T) {
			t.Parallel() // Keep idle/call sockets reading throughout the real timeouts.
			t.Run("VoiceTypedAndStop", s.voiceTypedStop)
			t.Run("VoCatMenus", func(t *testing.T) { s.menus(t, "esp-vocat", "MenusRound") })
			t.Run("Box3Menus", func(t *testing.T) { s.menus(t, "esp-box-3", "MenusRect") })
			t.Run("RawFramebuffer", s.rawFramebuffer)
			t.Run("IntercomReply", s.intercom)
			t.Run("Calls", s.calls)
			t.Run("Apps", s.apps)
			t.Run("BuilderAndParents", s.builderParents)
			t.Run("TimersMemoryHistory", s.data)
			t.Run("PhotosAndScopedTunnel", s.photosAndScope)
			t.Run("Diagnostics", s.diagnostics)
		})
	default:
		t.Skip("use scripts/check.sh --live or --e2e-real")
	}
}

func e2eFields(f wsFrame, op string, pairs ...string) bool {
	if !hasWS(op, "")(f) {
		return false
	}
	for i := 0; i < len(pairs); i += 2 {
		if !bytes.Contains(f.data, []byte(fmt.Sprintf(`"%s":%s`, pairs[i], pairs[i+1]))) {
			return false
		}
	}
	return true
}

func (c *e2eCat) op(t *testing.T, op string, pairs ...string) wsFrame {
	t.Helper()
	return c.expect(t, op+" "+strings.Join(pairs, " "), func(f wsFrame) bool { return e2eFields(f, op, pairs...) })
}

func (c *e2eCat) tapScene(t *testing.T, id, want string) string {
	t.Helper()
	c.tap(t, id)
	return c.scene(t, want)
}

func (c *e2eCat) note(t *testing.T, pcm []byte, target string) time.Time {
	t.Helper()
	event := object{"ev": "rec_start"}
	if target != "" {
		event["target"] = target
	}
	c.json(t, event)
	c.audio(t, 0x01, pcm)
	start := time.Now()
	c.json(t, object{"ev": "rec_end", "cancelled": false})
	return start
}

func (c *e2eCat) playback(t *testing.T, start time.Time) []byte {
	t.Helper()
	var first time.Duration
	var pcm []byte
	var audio int
	end := c.expect(t, "complete TTS playback", func(f wsFrame) bool {
		switch {
		case e2eFields(f, "tts", "state", `"start"`):
			if audio != 0 {
				t.Fatal("overlapping TTS playback")
			}
			audio = e2eNumber(testObject(t, f.data)["audio"])
		case isPlayback(f):
			if audio <= 0 {
				t.Fatal("PCM arrived before a numbered TTS start")
			}
			if first == 0 {
				first = time.Since(start)
			}
			pcm = append(pcm, f.data[1:]...)
		}
		return e2eFields(f, "tts", "state", `"end"`)
	})
	if !livePCMOK(pcm) || e2eNumber(testObject(t, end.data)["audio"]) != audio {
		t.Fatal("playback did not finish with matching audio ID and non-silent samples")
	}
	t.Logf("input end → first PCM %s (%d ms total audio)", first.Round(time.Millisecond), len(pcm)/(sampleRate*2/1000))
	c.ack(t, end)
	return pcm
}

func (s *e2eSuite) voiceTypedStop(t *testing.T) {
	t.Run("Voice", func(t *testing.T) {
		c := s.board(t, "esp-vocat", "Voice")
		c.timeout = 40 * time.Second
		c.mirrorOn(t, s)
		pcm := s.synth(t, e2eVoiceQuestion)
		c.tap(t, "cat-ring")
		c.op(t, "rec", "action", `"start"`)
		c.playback(t, c.note(t, pcm, ""))
		rows := s.history(t, c.id, "")
		if len(rows) != 2 || rows[0].Source != "voice" || !strings.Contains(strings.ToLower(rows[0].Text), "sky blue") ||
			rows[1].Role != "cat" || strings.TrimSpace(rows[1].Text) == "" {
			t.Fatal("voice did not persist a real transcript and assistant reply")
		}
		if !s.real && rows[1].Text != e2eModelReply {
			t.Fatal("unexpected deterministic voice reply")
		}
		t.Logf("Parakeet transcript: %s", rows[0].Text)
	})
	t.Run("Typed", func(t *testing.T) {
		c := s.browser(t, "Typed")
		c.timeout = 40 * time.Second
		start := time.Now()
		c.json(t, object{"ev": "text", "text": "Say hello in one short sentence, without tools."})
		c.playback(t, start)
		rows := s.history(t, c.id, "")
		if len(rows) != 2 || rows[0].Source != "typed" || !strings.HasPrefix(rows[0].Text, "Say hello") || rows[1].Role != "cat" {
			t.Fatal("typed /app turn did not write both canonical messages")
		}
	})
	t.Run("ThinkingStopAndStaleTap", func(t *testing.T) {
		if s.real {
			t.Skip("the deterministic model hold is exercised in --live")
		}
		c := s.board(t, "esp-vocat", "StopThink")
		c.mirrorOn(t, s)
		c.json(t, object{"ev": "text", "text": "Virtual test hold"})
		c.scene(t, `aria-label=\"Stop\"`)
		c.input(t, "tap", 180, 160, c.revision)
		c.input(t, "tap", 180, 315, c.revision-1)
		c.write(t, maskedFrame(wsPing, true, []byte("pre-stop")))
		c.expect(t, "face/stale taps ignored", func(f wsFrame) bool {
			if e2eFields(f, "chime", "kind", `"tap"`) || e2eFields(f, "chime", "kind", `"cancel"`) || hasWS("rec", "")(f) {
				t.Fatal("a face or stale Stop tap acted on the thinking turn")
			}
			return f.op == wsPong && string(f.data) == "pre-stop"
		})
		c.tapScene(t, "stop", "Tap 2×")
		c.tapScene(t, "stop", "Tap 1×")
		c.tap(t, "stop")
		c.op(t, "chime", "kind", `"cancel"`)
		c.scene(t, `data-id=\"menu\"`)
		if rows := s.history(t, c.id, ""); len(rows) != 1 || rows[0].Role != "user" {
			t.Fatal("cancelled thinking turn wrote an assistant reply")
		}
	})
	t.Run("SpeakingStop", func(t *testing.T) {
		if s.real {
			t.Skip("speaking Stop uses the deterministic utterance in --live")
		}
		c := s.board(t, "esp-box-3", "StopSpeak")
		c.mirrorOn(t, s)
		c.timeout = 40 * time.Second
		c.json(t, object{"ev": "text", "text": "Say one sentence about cats, without tools."})
		c.op(t, "tts", "state", `"start"`)
		c.expect(t, "spoken PCM", isPlayback)
		// The speaker still owns buffered audio until tts_ack. Test while it is
		// speaking without depending on where the paced stream paused.
		c.op(t, "tts", "state", `"end"`)
		c.scene(t, `aria-label=\"Stop\"`)
		c.input(t, "tap", 180, 160, c.revision)
		c.write(t, maskedFrame(wsPing, true, []byte("face-speaking")))
		c.expect(t, "face tap does not act while speaking", func(f wsFrame) bool {
			if e2eFields(f, "tts", "state", `"abort"`) || e2eFields(f, "rec", "action", `"start"`) {
				t.Fatal("face tap interrupted speaking")
			}
			return f.op == wsPong && string(f.data) == "face-speaking"
		})
		c.tap(t, "stop")
		c.op(t, "tts", "state", `"abort"`)
		c.scene(t, `data-id=\"menu\"`)
	})
}

func (s *e2eSuite) menus(t *testing.T, variant, label string) {
	c := s.board(t, variant, label)
	c.mirrorOn(t, s)
	s.browser(t, label+"Peer")
	first := c.tapScene(t, "menu", "menu-intercom")
	for _, id := range []string{"menu-call", "menu-games", "menu-history"} {
		if !strings.Contains(first, id) {
			t.Fatal("missing first page control:", id)
		}
	}
	for _, nav := range []struct{ id, want string }{
		{"menu-intercom", "Talk to…"}, {"menu-call", "Call"}, {"menu-games", "No saved games yet"}, {"menu-history", "No messages yet"},
	} {
		c.tapScene(t, nav.id, nav.want)
		c.tapScene(t, "menu-back", "menu-intercom")
	}
	c.tapScene(t, "menu-next", "menu-memories")
	c.tapScene(t, "menu-memories", "No memories yet")
	c.tapScene(t, "menu-back", "menu-memories")
	c.tapScene(t, "menu-voice", "voice-")
	for i, voice := range voiceCatalog {
		id := "voice-" + strings.ToLower(voice.Name)
		if !strings.Contains(c.svg, id) {
			c.tapScene(t, "menu-next", id)
		}
		c.tap(t, id)
		c.op(t, "chime", "kind", `"tap"`)
		got := testObject(t, s.request(t, "GET", "/api/devices", nil, 200))
		if !slices.ContainsFunc(got["devices"].([]any), func(v any) bool {
			return v.(object)["device_id"] == c.id && v.(object)["voice"] == voice.ID
		}) {
			t.Fatalf("voice choice %d (%s) did not persist", i, voice.Name)
		}
	}
	c.tapScene(t, "menu-back", "menu-volume")
	c.tapScene(t, "menu-volume", "vol-plus")
	c.tap(t, "vol-plus")
	c.op(t, "volume", "persist", "true")
	c.tapScene(t, "menu-back", "menu-volume")
	c.tapScene(t, "menu-back", "cat-ring")
	hasColor := func(r image.Rectangle) bool {
		seen := map[uint16]bool{}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				seen[c.pixels[y*c.width+x]] = true
			}
		}
		return len(seen) >= 3
	}
	if c.codecs[1] == 0 || !hasColor(image.Rect(c.width/4, c.high/4, 3*c.width/4, 3*c.high/4)) {
		t.Fatal("decoded framebuffer has no face artwork or RLE565 packet")
	}
	if variant == "esp-vocat" {
		c.json(t, object{"ev": "status", "battery_pct": 48, "battery_mv": 3840, "charging": false, "volume": 70,
			"wifi_rssi": -51, "rssi": -51, "free_heap": 110000, "min_free_heap": 100000, "free_psram": 1900000, "uptime_s": 419, "cpu_pct": 25})
		f := c.expect(t, "battery dirty rectangle", func(f wsFrame) bool {
			if !isDisplay(f) {
				return false
			}
			r, _, _ := decodeDisplay(t, f)
			return r.Dx() <= 50 && r.Dy() <= 25 && (r.Min.X < 150 || r.Min.X > 210)
		})
		rect, _, _ := decodeDisplay(t, f)
		t.Logf("firmware telemetry produced native dirty rectangle %v", rect)
	}
	t.Logf("%d×%d native: RGB565=%d, RLE565=%d", c.width, c.high, c.codecs[0], c.codecs[1])
}

func (s *e2eSuite) rawFramebuffer(t *testing.T) {
	c := s.board(t, "esp-vocat", "RawPixels")
	c.mirrorOn(t, s)
	// Two otherwise-identical scenes make an incompressible dirty patch.
	const background = `<svg><rect width="360" height="360" fill="black"/><text x="180" y="95">Raw pixels</text>`
	s.tool(t, "app", object{"devices": c.id, "svg": background + "</svg>"}, false)
	c.scene(t, "Raw pixels")
	var mosaic strings.Builder
	mosaic.WriteString(background)
	for y := range 12 {
		for x := range 12 {
			rgb := (uint32(1+x+12*y) * 0x987ab5) & 0xffffff
			fmt.Fprintf(&mosaic, `<rect x="%d" y="%d" width="1" height="1" fill="#%06x"/>`, 170+x, 145+y, rgb)
		}
	}
	mosaic.WriteString("</svg>")
	s.tool(t, "app", object{"devices": c.id, "svg": mosaic.String()}, false)
	f := c.expect(t, "raw RGB565 mosaic rectangle", func(f wsFrame) bool { return isDisplay(f) && f.data[9] == 0 })
	rect, _, pixels := decodeDisplay(t, f)
	if rect != image.Rect(170, 145, 182, 157) || pixels[0] != uint16(0x98>>3)<<11|uint16(0x7a>>2)<<5|uint16(0xb5>>3) {
		t.Fatal("wrong raw RGB565 dirty patch geometry or byte order:", rect)
	}
	t.Logf("native RGB565 patch %v (%d bytes)", rect, len(f.data))
}

func (s *e2eSuite) intercom(t *testing.T) {
	a, b := s.browser(t, "NoteA"), s.browser(t, "NoteB")
	pcm := s.synth(t, "This is a voice note about a purple kite.")
	start := a.note(t, pcm, b.id)
	b.op(t, "chime", "kind", `"intercom"`)
	// Intercom applies the published voice normalization after the spoken
	// sender-name prefix. It must not transcribe/resynthesize the note.
	spoken := b.playback(t, start)
	if !bytes.HasSuffix(spoken, NormalizeVoicePCM(pcm)) {
		t.Fatal("intercom lost the normalized original recording")
	}
	b.scene(t, "reply")
	b.tap(t, "reply")
	if start := testObject(t, b.op(t, "rec", "action", `"start"`).data); start["target"] != a.id {
		t.Fatal("Reply did not target the original sender")
	}
	reply := s.synth(t, "I received your voice note.")
	now := b.note(t, reply, a.id)
	a.op(t, "chime", "kind", `"intercom"`)
	if !bytes.HasSuffix(a.playback(t, now), NormalizeVoicePCM(reply)) {
		t.Fatal("Reply did not relay the recorded audio back")
	}
}

func e2eRing(t *testing.T, a, b *e2eCat) time.Time {
	t.Helper()
	start := time.Now()
	a.json(t, object{"ev": "call", "target": b.id})
	a.op(t, "chime", "kind", `"ring"`)
	b.op(t, "chime", "kind", `"ring"`)
	b.scene(t, "call-answer")
	return start
}

func e2eStartCall(t *testing.T, a, b *e2eCat) int {
	t.Helper()
	first, second := a.op(t, "call", "state", `"start"`), b.op(t, "call", "state", `"start"`)
	id := e2eNumber(testObject(t, first.data)["call"])
	for _, f := range []wsFrame{first, second} {
		msg := testObject(t, f.data)
		if id == 0 || e2eNumber(msg["call"]) != id || e2eNumber(msg["mic_prefix"]) != 0x04 {
			t.Fatal("peers did not negotiate a matching tagged firmware call")
		}
	}
	a.scene(t, "call-mute")
	b.scene(t, "call-mute")
	return id
}

func e2eEndCall(t *testing.T, a, b *e2eCat, reason string) {
	t.Helper()
	for _, c := range []*e2eCat{a, b} {
		c.op(t, "call", "state", `"end"`, "reason", fmt.Sprintf("%q", reason))
	}
}

func (s *e2eSuite) calls(t *testing.T) {
	for _, variant := range []string{"WebWeb", "WebBoard"} {
		t.Run(variant, func(t *testing.T) {
			a := s.browser(t, "CallFrom")
			var b *e2eCat
			if variant == "WebWeb" {
				b = s.browser(t, "CallTo")
			} else {
				b = s.board(t, "esp-box-3", "CallBoard")
				b.mirrorOn(t, s)
			}
			ring := e2eRing(t, a, b)
			b.tap(t, "call-answer")
			e2eStartCall(t, a, b)
			t.Logf("ring → connected %s", time.Since(ring).Round(time.Millisecond))
			pcm := bytes.Repeat([]byte{100, 5}, callPCMBytes/2)
			relay := func(from, to *e2eCat, data []byte) {
				t.Helper()
				start := time.Now()
				from.write(t, maskedFrame(wsBinary, true, append([]byte{0x04}, data...)))
				if got := to.expect(t, "relayed call PCM", isPlayback); !bytes.Equal(got.data[1:], data) {
					t.Fatal("call relayed unexpected or mutated PCM")
				}
				t.Logf("tagged microphone → peer PCM %s", time.Since(start).Round(time.Microsecond))
			}
			relay(a, b, pcm)
			relay(b, a, pcm)
			b.tap(t, "call-mute")
			b.op(t, "call", "muted", "true")
			b.scene(t, "Unmute")
			b.write(t, maskedFrame(wsBinary, true, append([]byte{0x04}, pcm...)))
			b.syncFrames(t)
			relay(a, b, pcm) // Mute does not silence downlink.
			b.tap(t, "call-mute")
			b.op(t, "call", "muted", "false")
			relay(b, a, bytes.Repeat([]byte{5, 100}, callPCMBytes/2))
			b.tap(t, "call-hangup")
			e2eEndCall(t, a, b, "hung up")
			a.expect(t, "call history event", func(f wsFrame) bool {
				return f.op == wsText && bytes.Contains(f.data, []byte(`"type":"chat"`)) &&
					bytes.Contains(f.data, []byte(`"source":"call"`))
			})
			if !slices.ContainsFunc(s.history(t, a.id, ""), func(row chatEntry) bool {
				return row.Source == "call" && strings.Contains(row.Text, "Voice call with")
			}) {
				t.Fatal("completed call did not enter canonical history")
			}
		})
	}
	t.Run("BusyDeclineGlare", func(t *testing.T) {
		a, b, third := s.browser(t, "GlareA"), s.browser(t, "GlareB"), s.browser(t, "Busy")
		e2eRing(t, a, b)
		third.json(t, object{"ev": "call", "target": b.id})
		third.scene(t, "busy.")
		b.tap(t, "call-decline")
		e2eEndCall(t, a, b, "declined")
		e2eRing(t, a, b)
		b.json(t, object{"ev": "call", "target": a.id})
		e2eStartCall(t, a, b) // The reciprocal request answers, not starts a second call.
		a.tap(t, "call-hangup")
		e2eEndCall(t, a, b, "hung up")
	})
}

func (s *e2eSuite) callTimeout(t *testing.T) {
	a, b := s.browser(t, "TimeoutA"), s.browser(t, "TimeoutB")
	a.timeout, b.timeout = 32*time.Second, 32*time.Second
	start := e2eRing(t, a, b)
	e2eEndCall(t, a, b, "no answer")
	if elapsed := time.Since(start); elapsed < 24*time.Second || elapsed > 29*time.Second {
		t.Fatalf("unexpected no-answer timing: %s", elapsed)
	} else {
		t.Logf("ring → no answer %s", elapsed.Round(time.Millisecond))
	}
}

func (s *e2eSuite) idleWake(t *testing.T, c *e2eCat, start time.Time) {
	c.timeout = 130 * time.Second
	c.op(t, "power", "brightness", "12")
	if elapsed := time.Since(start); elapsed < 118*time.Second || elapsed > 125*time.Second {
		t.Fatalf("unexpected idle sleep timing: %s", elapsed)
	} else {
		t.Logf("idle → panel-off power %s", elapsed.Round(time.Millisecond))
	}
	c.syncFrames(t)
	start = time.Now()
	c.input(t, "tap", 180, 145, c.revision)
	c.op(t, "power", "brightness", "85")
	c.write(t, maskedFrame(wsPing, true, []byte("wake")))
	c.expect(t, "wake without also starting Talk", func(f wsFrame) bool {
		if e2eFields(f, "rec", "action", `"start"`) {
			t.Fatal("wake tap also started recording")
		}
		return f.op == wsPong && string(f.data) == "wake"
	})
	t.Logf("wake touch → active power %s", time.Since(start).Round(time.Millisecond))
}

func e2ePhotosFixture(t *testing.T, local string) {
	t.Helper()
	// The subprocess opens and reconciles the generated Ente export/index.
	// It cannot authenticate or fetch originals; the derivative cache is seeded.
	e2eWrite(t, filepath.Join(local, "bin/ente-photo-job"), []byte("#!/bin/sh\nexit 1\n"), 0o700)
	p, err := openPhotoLibrary(t.Context(), local, "")
	if err != nil {
		t.Fatal("cannot create isolated photo index")
	}
	defer p.close()
	ingestTestPhotos(t, p, fakeEnteSnapshot(), time.Now())
	files, err := p.search(t.Context(), "beach", "", 8)
	if err != nil || len(files) != 2 {
		t.Fatal("synthetic index did not ingest")
	}
	for i, f := range files {
		col := []imagecolor.RGBA{{R: 250, A: 255}, {B: 250, A: 255}}[i]
		if err := p.storeDerivative(t.Context(), f.photoKey, testJPEG(t, col)); err != nil {
			t.Fatal("cannot seed isolated derivative")
		}
	}
}

func (s *e2eSuite) photosAndScope(t *testing.T) {
	for _, g := range []struct{ variant, name string }{{"esp-vocat", "PhotosRound"}, {"esp-box-3", "PhotosRect"}} {
		t.Run(g.name, func(t *testing.T) {
			c := s.board(t, g.variant, g.name)
			c.mirrorOn(t, s)
			c.mirror.timeout = 45 * time.Second
			path := "/api/library?device_id=" + c.id + "&q=beach"
			results := testObject(t, s.request(t, "GET", path, nil, 200))
			items, _ := results["items"].([]any)
			if results["ready"] != true || len(items) != 2 {
				t.Fatal("synthetic metadata query did not return two images")
			}
			raw := s.request(t, "GET", "/api/library/thumbnail/"+str(items[0].(object)["handle"]), nil, 200)
			if len(raw) < 100 || raw[0] != 0xff || raw[1] != 0xd8 {
				t.Fatal("synthetic thumbnail is not JPEG data")
			}
			prompt := e2ePhotoPrompt + " in a slideshow."
			if s.real {
				handles, _ := json.Marshal([]string{str(items[0].(object)["handle"]), str(items[1].(object)["handle"])})
				prompt += " In one Code Mode execute, run the following, replacing ACCESS with the 32-character local photos access value from the turn header. Do not print access or call any other tool.\n" +
					fmt.Sprintf("return await tools.%s.app({devices:%q,template:\"slideshow\",data:{handles:%s,access:ACCESS}});", s.model.alias, c.id, handles)
				t.Cleanup(func() {
					if t.Failed() {
						s.model.mu.Lock()
						defer s.model.mu.Unlock()
						t.Logf("isolated MCP results: %v", s.model.mcpAudit)
					}
				})
			}
			c.json(t, object{"ev": "text", "text": prompt})
			c.scene(t, "photo:lib-")
			red565 := func(p uint16) bool { return p>>11 > 25 && p&31 < 4 }
			blue565 := func(p uint16) bool { return p>>11 < 4 && p&31 > 25 }
			// Picture area includes the native center on both displays.
			center := c.high/2*c.width + c.width/2
			if !red565(c.pixels[center]) {
				t.Fatal("first synthetic photo is missing from native framebuffer")
			}
			c.tap(t, "photo-next")
			c.scene(t, "2 / 2", "photo:lib-")
			if !blue565(c.pixels[center]) {
				t.Fatal("Next did not paint the second photo")
			}
			s.request(t, "POST", "/api/library?device_id="+c.id, object{"op": "visibility", "hidden": true}, 200)
			c.scene(t, "cat-ring")
			hidden := testObject(t, s.request(t, "GET", path, nil, 200))
			if hidden["hidden"] != true || len(hidden["items"].([]any)) != 0 {
				t.Fatal("per-cat hide did not revoke the slideshow and query")
			}
		})
	}
	t.Run("Scope", func(t *testing.T) {
		c := s.browser(t, "Share")
		c.testWS.conn.Close()
		other := s.browser(t, "NotShared")
		scope := s.scope(t, c.id)
		get := func(path string, code int) []byte { return e2eRequest(t, scope, "GET", path, "", nil, code) }
		get("/app", 200)
		info := testObject(t, get("/api/scope", 200))
		if info["device_id"] != c.id || info["scoped"] != true {
			t.Fatal("scoped listener resolved the wrong virtual cat")
		}
		for _, path := range []string{"/api/devices", "/api/parent", "/api/diagnostics", "/mcp",
			"/api/library?device_id=" + c.id, "/api/library/thumbnail/photo-12345678", "/app/photo-library.js"} {
			get(path, 404)
		}
		get("/api/devices/"+c.id+"/messages", 200)
		get("/api/devices/"+other.id+"/messages", 403)
		ws := testConnectWS(t, scope.base, scope.token, true)
		ws.json(t, object{"ev": "hello", "role": "web", "device_id": c.id, "caps": object{"call": true, "drag": true, "bridge_endpoint": true}})
		client := &e2eCat{testWS: ws, id: c.id, name: c.name}
		client.timeout = 20 * time.Second
		client.scene(t, "cat-ring")
		ws.json(t, object{"ev": "watch", "device_id": other.id})
		client.op(t, "error", "status", "403")
		if !s.real {
			ws.json(t, object{"ev": "text", "text": e2ePhotoPrompt})
			client.playback(t, time.Now())
			s.model.mu.Lock()
			capability, found := s.model.photo[c.id]
			defer s.model.mu.Unlock()
			if capability {
				t.Fatal("scoped prompt received a photo access grant")
			}
			if !found {
				t.Fatal("scoped prompt did not reach the deterministic upstream")
			}
		}
	})
}

func (s *e2eSuite) diagnostics(t *testing.T) {
	if app := s.request(t, "GET", "/app", nil, 200); !bytes.Contains(app, []byte(`id="stage"`)) {
		t.Fatal("browser entry point is not the cat app")
	}
	for _, asset := range []string{"client.js", "style.css", "mic.js"} {
		s.request(t, "GET", "/app/"+asset, nil, 200)
	}
	unauthorized := s.liveClient
	unauthorized.token = ""
	e2eRequest(t, unauthorized, "GET", "/api/diagnostics", "", nil, 401)
	diag := testObject(t, s.request(t, "GET", "/api/diagnostics", nil, 200))
	for _, key := range []string{"bridge", "devices", "services", "recent_turns"} {
		if _, ok := diag[key]; !ok {
			t.Errorf("missing diagnostic section %s", key)
		}
	}
	probe := liveJSON[object](t, s.liveClient, "/api/diagnostics/stt", nil, []byte{}, "", nil)
	if !strings.EqualFold(str(probe["transcript"]), "The moon reflects sunlight.") || e2eNumber(probe["clip_ms"]) < 500 {
		t.Fatal("isolated STT diagnostic could not transcribe the reference clip")
	}
	t.Logf("diagnostic Parakeet: %v ms on %v ms PCM", probe["stt_ms"], probe["clip_ms"])
}

func (s *e2eSuite) historyPage(t *testing.T, id string, cursor *historyCursor) historyPage {
	t.Helper()
	q := url.Values{"limit": {"1"}}
	if cursor != nil {
		q.Set("before_ts", fmt.Sprint(cursor.TS))
		q.Set("before_id", cursor.ID)
	}
	var page historyPage
	if err := json.Unmarshal(s.request(t, "GET", "/api/devices/"+id+"/messages?"+q.Encode(), nil, 200), &page); err != nil {
		t.Fatal("could not decode history page")
	}
	return page
}
