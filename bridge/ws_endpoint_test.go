package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	return &endpointSettings{newVAD: func() endpointVAD { return &testEndpointVAD{} }}
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
	clips := make(chan []byte, 4)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.endpoint = testEndpointSettings()
		b.stt = func(_ context.Context, pcm []byte) (string, error) {
			clips <- slices.Clone(pcm)
			return "", nil
		}
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
	select {
	case got := <-clips:
		if !bytes.Equal(got, frame) {
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
	case got := <-clips:
		voices := bytes.Count(got, frame)
		if voices != 2 {
			t.Fatalf("whole clip did not contain exactly both clauses (got %d)", voices)
		}
	default:
		t.Fatal("automatic endpoint did not transcribe")
	}
	if len(clips) != 0 {
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
		b.stt = func(context.Context, []byte) (string, error) { return "and add stars", nil }
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
