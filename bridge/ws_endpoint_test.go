package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type testEndpointVAD struct{ tail int }

func (v *testEndpointVAD) detect(frame []byte) bool {
	if frame[0] != 0 {
		v.tail = int(vadTail / endpointFrame)
		return true
	}
	if v.tail > 0 {
		v.tail--
		return true
	}
	return false
}
func (*testEndpointVAD) close() {}

func testEndpointSettings() *endpointSettings {
	return &endpointSettings{newVAD: func() endpointVAD { return &testEndpointVAD{} }, streams: make(chan struct{}, 1)}
}

func TestEndpointWaitsForContinuation(t *testing.T) {
	for _, text := range []string{"", "Once upon a time there was a frog, and then.", "Could you set a timer for?", "Can you draw a dragon?"} {
		var p endpointPolicy
		if got := p.step(time.Second, true, text); got != endpointWait {
			t.Fatal(got)
		}
		if got := p.step(5*time.Second, false, text); got != endpointWait {
			t.Fatalf("%q prematurely committed at 4-second pause", text)
		}
		p.step(5200*time.Millisecond, true, text) // Voice resumes.
		if got := p.step(6*time.Second, false, text); got != endpointWait {
			t.Fatalf("%q kept the old speech deadline", text)
		}
		if got := p.step(9600*time.Millisecond, false, text); got != endpointCommit {
			t.Fatalf("%q did not commit after the new end", text)
		}
	}
	var quick, empty endpointPolicy
	quick.step(time.Second, true, "Start a timer for ninety seconds")
	if got := quick.step(1600*time.Millisecond, false, "Start a timer for ninety seconds"); got != endpointCommit {
		t.Fatalf("complete timer did not skip the long wait: %v", got)
	}
	if got := empty.step(endpointIdle, false, ""); got != endpointDiscard {
		t.Fatalf("initial silence did not discard: %v", got)
	}
}

func TestEndpointAccumulatorPreservesOriginalPCM(t *testing.T) {
	var u utterance
	vad := &testEndpointVAD{}
	frame := bytes.Repeat([]byte{46, 0}, endpointBytes/2)
	quiet := make([]byte, endpointBytes)
	u.add(frame[:50], vad, "")
	u.add(frame[50:], vad, "")
	for range 200 {
		if got := u.add(quiet, vad, "Once upon a time"); got != endpointWait {
			t.Fatalf("unexpected decision %v", got)
		}
	}
	u.add(frame, vad, "")
	want := slices.Concat(frame, make([]byte, 200*endpointBytes), frame)
	if !bytes.Equal(u.clip(), want) {
		t.Fatal("accumulated PCM changed")
	}
	if u = (utterance{}); u.clip() != nil {
		t.Fatal("empty capture would enter STT")
	}
}

func TestEndpointWebSendCancelAndAuto(t *testing.T) {
	wav := make(chan []byte, 4)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.endpoint = testEndpointSettings()
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/streams":
				http.Error(w, "no live model", http.StatusServiceUnavailable)
			case "/transcriptions":
				pcm, _ := io.ReadAll(r.Body)
				wav <- pcm
				_, _ = io.WriteString(w, `{"transcript":""}`)
			default:
				t.Errorf("unexpected Hex request %s", r.URL.Path)
			}
		})
	})
	const id = "phone-e021"
	d := testConnectWS(t, base, "unit", false)
	d.json(t, object{"ev": "hello", "role": "web", "device_id": id, "character": "Echo", "caps": object{"bridge_endpoint": true}})
	d.until(t, hasWS("capabilities", `"bridge_endpoint":true`))
	start := func() {
		d.json(t, object{"ev": "rec_request"})
		d.until(t, hasWS("rec", `"endpoint":"bridge"`))
		d.json(t, object{"ev": "rec_start"})
	}
	appendPCM := func(p []byte) {
		d.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, p...)))
	}
	frame := bytes.Repeat([]byte{17, 0}, endpointBytes/2)
	start()
	appendPCM(frame)
	d.json(t, object{"ev": "rec_end", "cancelled": true})
	d.until(t, hasWS("mode", `"mode":"idle"`))
	start()
	appendPCM(frame)
	d.json(t, object{"ev": "rec_end"}) // Browser Send now flushes then sends rec_end.
	d.until(t, hasWS("mode", `"mode":"idle"`))
	pad := make([]byte, len(hexSilence))
	select {
	case got := <-wav:
		if !bytes.Equal(got, WrapWAV(slices.Concat(pad, frame, pad))) {
			t.Fatal("Send did not commit the original single frame exactly once")
		}
	default:
		t.Fatal("Send did not transcribe")
	}
	start()
	appendPCM(frame)
	quiet := make([]byte, endpointBytes*4)
	for range 55 {
		appendPCM(quiet)
	}
	appendPCM(frame) // 4.4 seconds of silence is not an endpoint in unknown speech.
	d.barrier(t)
	b.ws.mu.Lock()
	stillListening := b.ws.devices[id].recording.phase == captureOpen
	b.ws.mu.Unlock()
	if !stillListening {
		t.Fatal("bridge stopped before the continuation")
	}
	for range 60 {
		appendPCM(quiet)
	}
	d.until(t, hasWS("rec", `"action":"stop"`))
	d.json(t, object{"ev": "rec_end"})
	d.until(t, hasWS("mode", `"mode":"idle"`))
	select {
	case got := <-wav:
		voices := bytes.Count(got, frame)
		if voices != 2 {
			t.Fatalf("whole clip fallback did not contain exactly both clauses (got %d)", voices)
		}
	default:
		t.Fatal("automatic endpoint did not transcribe")
	}
	if len(wav) != 0 {
		t.Fatal("capture was transcribed more than once")
	}
	b.diag.mu.Lock()
	n := len(b.diag.metrics[diagEndpoint].items)
	b.diag.mu.Unlock()
	if n != 1 {
		t.Fatalf("got %d end-to-commit samples, want 1 for the observed endpoint", n)
	}
}

func TestEndpointHardwareNegotiation(t *testing.T) {
	for _, tc := range []struct{ model, cap, want bool }{
		{false, true, false}, {true, false, false}, {true, true, true},
	} {
		t.Run(fmt.Sprintf("model=%t cap=%t", tc.model, tc.cap), func(t *testing.T) {
			_, base := wsHarness(t, func(b *bridge, _ string) {
				if tc.model {
					b.endpoint = testEndpointSettings()
				}
			})
			d := testConnectWS(t, base, "unit", false)
			d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": "echoear-ep", "character": "Echo", "caps": object{"bridge_endpoint": tc.cap}})
			d.until(t, isDisplay)
			d.json(t, object{"ev": "tap", "x": 180, "y": 180})
			f := d.until(t, hasWS("rec", `"action":"start"`))
			if got := bytes.Contains(f.data, []byte(`"endpoint":"bridge"`)); got != tc.want {
				t.Fatalf("negotiation: %s", f.data)
			}
		})
	}
}

func TestTargetedRecordingKeepsLegacyEndpoint(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.endpoint = testEndpointSettings() })
	const id = "echoear-intercom-ep"
	d := testConnectWS(t, base, "unit", false)
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Echo", "caps": object{"bridge_endpoint": true}})
	d.until(t, isDisplay)
	b.ws.mu.Lock()
	cat := b.ws.devices[id]
	b.ws.startRecording(cat, "all", time.Now(), true)
	b.ws.mu.Unlock()
	start := d.until(t, hasWS("rec", `"target":"all"`))
	if bytes.Contains(start.data, []byte(`"endpoint"`)) {
		t.Fatal("intercom should still use the client VAD")
	}
}

func TestEndpointHoldsUnpublishedPCMUntilMicCloses(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.endpoint = testEndpointSettings() })
	const id = "phone-e023"
	d := testConnectWS(t, base, "unit", false)
	d.json(t, object{"ev": "hello", "role": "web", "device_id": id, "character": "Echo", "caps": object{"bridge_endpoint": true}})
	d.until(t, hasWS("capabilities", `"bridge_endpoint":true`))
	d.json(t, object{"ev": "rec_request"})
	d.until(t, hasWS("rec", `"endpoint":"bridge"`))
	d.json(t, object{"ev": "rec_start"})
	d.barrier(t)
	stream := newAudioStream()
	stream.append([]byte{8, 0, 9, 0})
	stream.finish(nil)
	b.ws.mu.Lock()
	cat := b.ws.devices[id]
	b.ws.play(cat, stream)
	b.ws.mu.Unlock()
	d.json(t, object{"ev": "rec_end", "cancelled": true})
	cleared := false
	d.until(t, func(f wsFrame) bool {
		if hasWS("chime", `"kind":"cancel"`)(f) {
			cleared = true
		}
		if isPlayback(f) && !cleared {
			t.Fatal("PCM escaped while recording remained open")
		}
		return isPlayback(f)
	})
}

func TestStopDiscardsQueuedEndpoint(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.endpoint = testEndpointSettings() })
	const id = "phone-e024"
	d := testConnectWS(t, base, "unit", false)
	d.hello(t, id, "Echo")
	b.ws.mu.Lock()
	cat := b.ws.devices[id]
	ctx, cancel := context.WithCancel(cat.client.ctx)
	cat.busy = true
	cat.queuedVoice = &endpointCapture{ctx: ctx, cancel: cancel}
	for range 3 {
		b.ws.tapStop(cat, time.Now())
	}
	busy, queued := cat.busy, cat.queuedVoice
	b.ws.mu.Unlock()
	if ctx.Err() == nil || busy || queued != nil {
		t.Fatal("explicit Stop did not discard both the active and queued turns")
	}
}

func TestEndpointQueuesWhileOpenCodeOwnsTurn(t *testing.T) {
	const sid, id = "ses-followup", "phone-e022"
	prompts := make(chan string, 2)
	finishFirst := make(chan struct{})
	var count atomic.Int32
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.endpoint = testEndpointSettings()
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/streams" {
				http.Error(w, "no live model", http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"transcript":"and add stars"}`)
		})
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session", "/api/session/" + sid:
				sendJSON(w, 200, object{"data": object{"id": sid}})
			case "/api/session/" + sid + "/agent", "/api/session/" + sid + "/model":
				w.WriteHeader(http.StatusNoContent)
			case "/api/session/" + sid + "/prompt":
				var input struct{ Text, Delivery string }
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input.Delivery == "steer" {
					t.Error("unverified steering must stay off")
				}
				i := count.Add(1)
				prompts <- strings.TrimSpace(input.Text[strings.LastIndex(input.Text, "\n")+1:])
				fmt.Fprintf(w, `{"data":{"id":"prompt%d"}}`, i)
			case "/api/experimental/session/" + sid + "/wait":
				select {
				case <-finishFirst:
				case <-r.Context().Done():
				}
				w.WriteHeader(http.StatusNoContent)
			case "/api/session/" + sid + "/message":
				fmt.Fprintf(w, `{"data":[{"id":"answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Ready."}]},{"id":"prompt%d","type":"user"}]}`, count.Load())
			case "/api/session/" + sid + "/interrupt":
				t.Error("a voice follow-up must not interrupt the prompt gate owner")
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		})
	})
	d := testConnectWS(t, base, "unit", false)
	d.json(t, object{"ev": "hello", "role": "web", "device_id": id, "character": "Echo", "caps": object{"bridge_endpoint": true}})
	d.until(t, hasWS("capabilities", `"bridge_endpoint":true`))
	d.json(t, object{"ev": "text", "text": "Draw a dragon"})
	expectValue(t, prompts, "Draw a dragon")
	d.json(t, object{"ev": "rec_request"})
	d.until(t, hasWS("rec", `"endpoint":"bridge"`))
	d.json(t, object{"ev": "rec_start"})
	d.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix, 18, 0}, make([]byte, endpointBytes-2)...)))
	d.json(t, object{"ev": "rec_end"})
	d.barrier(t)
	select {
	case <-prompts:
		t.Fatal("follow-up bypassed gate")
	default:
	}
	close(finishFirst)
	expectValue(t, prompts, "and add stars")
	d.until(t, hasWS("mode", `"mode":"idle"`))
	eventually(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return !b.ws.devices[id].busy
	})
	if count.Load() != 2 {
		t.Fatal("prompt was replayed")
	}
}

func TestCaptureHexPostsOneOwnedStream(t *testing.T) {
	const owner = "fake-stream-owner"
	audio := make(chan []byte, 2)
	calls := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/streams":
			calls <- "open"
			_, _ = io.WriteString(w, `{"id":7,"ownerToken":"`+owner+`","partials":true,"leaseMs":10000}`)
		case "/streams/7/events":
			if r.Header.Get("X-Hex-Stream-Token") != owner {
				t.Error("missing owner token")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"partial","stable":"Start a timer","tentative":"for ninety seconds"}`+"\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/streams/7/audio":
			if r.Header.Get("X-Hex-Stream-Token") != owner || r.Header.Get("Content-Type") != "application/octet-stream" {
				t.Error("bad audio request headers")
			}
			pcm, _ := io.ReadAll(r.Body)
			audio <- pcm
			w.WriteHeader(http.StatusNoContent)
		case "/streams/7/finish":
			calls <- "finish"
			_, _ = io.WriteString(w, `{"text":"Start a timer for ninety seconds."}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := &bridge{endpoint: testEndpointSettings(), hex: hexClient{upstream: upstream{"Hex", server.Client(), fixedEndpoint{base: server.URL}}}}
	h := &wsHub{b: b}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stream := h.startCaptureHex(ctx)
	expectValue(t, calls, "open")
	if second := h.startCaptureHex(ctx); second != nil {
		t.Fatal("allowed simultaneous streams")
	}
	eventually(t, func() bool { return stream.hint.Load() != nil })
	if got := *stream.hint.Load(); got != "Start a timer for ninety seconds" {
		t.Fatal(got)
	}
	pcm := []byte{34, 0, 17, 0}
	stream.push(pcm)
	close(stream.audio)
	text, ok := stream.final(ctx)
	if !ok || text != "Start a timer for ninety seconds." {
		t.Fatalf("lost authoritative final: %q", text)
	}
	expectValue(t, calls, "finish")
	select {
	case got := <-audio:
		if !bytes.Equal(got, pcm) {
			t.Fatal("mutated streaming PCM")
		}
	default:
		t.Fatal("did not upload original PCM")
	}
}
