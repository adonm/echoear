package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const callCake = "phone-000a"
const callMint = "phone-000b"

func callClient(t *testing.T, b *bridge, base, id, name string) *testWS {
	t.Helper()
	tx, err := b.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = writeCat(t.Context(), tx, id, legacyCat(name, name)); err == nil {
		err = tx.Commit()
	}
	if err != nil {
		t.Fatal(err)
	}
	c := testConnectWS(t, base, "unit", false)
	c.json(t, object{"ev": "hello", "role": "web", "device_id": id, "character": name,
		"caps": object{"call": true, "drag": true}})
	c.until(t, hasWS("svg", ""))
	return c
}

func tapCallControl(t *testing.T, c *testWS, id string) {
	t.Helper()
	c.json(t, object{"ev": "tap", "id": id, "x": 180, "y": 180})
}

func checkCallStates(t *testing.T, h *wsHub, a, b callState) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, want := range map[string]callState{callCake: a, callMint: b} {
		d := h.devices[id]
		if d == nil {
			if want != callNone {
				t.Fatalf("%s disconnected; wanted %v", id, want)
			}
		} else if d.callState != want || want == callNone && (d.callID != 0 || d.callPeer != "" || d.callMuted || d.callCancel != nil) {
			t.Fatalf("%s has state=%v, id=%d, peer=%q; wanted %v", id, d.callState, d.callID, d.callPeer, want)
		}
	}
}

func TestWSCallDuplex(t *testing.T) {
	var stt atomic.Int32
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) { stt.Add(1) })
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.json(t, object{"ev": "call", "target": callMint})
	cake.until(t, hasWS("chime", `"kind":"ring"`))
	cake.until(t, hasWS("svg", "Calling Mint"))
	mint.until(t, hasWS("chime", `"kind":"ring"`))
	mint.until(t, hasWS("svg", "call-answer"))
	checkCallStates(t, b.ws, callRingingOut, callRingingIn)

	tapCallControl(t, mint, "call-answer")
	for _, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"state":"start"`))
		c.until(t, hasWS("svg", "call-mute"))
	}
	checkCallStates(t, b.ws, callActive, callActive)
	pcm := bytes.Repeat([]byte{100, 5}, callPCMBytes/2)
	sendPCM := func(c *testWS, sample []byte) {
		c.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, sample...)))
	}
	for _, pair := range [][2]*testWS{{cake, mint}, {mint, cake}} {
		sendPCM(pair[0], pcm)
		if got := pair[1].until(t, isPlayback); !bytes.Equal(got.data[1:], pcm) {
			t.Fatal("call changed the relayed PCM")
		}
	}

	tapCallControl(t, cake, "call-mute")
	cake.until(t, hasWS("call", `"muted":true`))
	cake.until(t, hasWS("svg", "Unmute"))
	sendPCM(cake, pcm)
	cake.barrier(t)
	// A mute blocks uplink only.
	sendPCM(mint, pcm)
	cake.until(t, isPlayback)
	tapCallControl(t, cake, "call-mute")
	cake.until(t, hasWS("call", `"muted":false`))
	probe := bytes.Repeat([]byte{5, 100}, callPCMBytes/2)
	sendPCM(cake, probe)
	if got := mint.until(t, isPlayback); !bytes.Equal(got.data[1:], probe) {
		t.Fatal("received PCM queued while caller was muted")
	}
	// A touch on the avatar must neither end the call nor start a recording.
	cake.json(t, object{"ev": "tap", "x": 180, "y": 113})
	cake.barrier(t)
	checkCallStates(t, b.ws, callActive, callActive)
	b.ws.mu.Lock()
	for _, d := range b.ws.devices {
		if d.mic != nil || d.recOpen || d.busy {
			t.Error("call audio entered the assistant turn path")
		}
	}
	b.ws.mu.Unlock()

	tapCallControl(t, mint, "call-hangup")
	for i, c := range []*testWS{cake, mint} {
		c.until(t, hasWS("call", `"state":"end"`))
		c.until(t, hasWS("chime", `"kind":"cancel"`))
		peer := []string{"Mint", "Cake"}[i]
		var idle, noted bool
		c.until(t, func(f wsFrame) bool {
			idle = idle || hasWS("svg", "cat-ring")(f)
			noted = noted || f.op == wsText && bytes.Contains(f.data, []byte("Voice call with "+peer+" (0:"))
			return idle && noted
		})
		id := []string{callCake, callMint}[i]
		history, err := b.history.list(t.Context(), id)
		if err != nil || len(history) != 1 || history[0].Source != "call" {
			t.Fatalf("%s call history: %v %v", id, history, err)
		}
	}
	checkCallStates(t, b.ws, callNone, callNone)
	if stt.Load() != 0 {
		t.Fatal("duplex call invoked Hex")
	}
}

func TestWSCallDeclineBusyTimeoutAndDisconnect(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	plum := callClient(t, b, base, "phone-000c", "Plum")
	cake.json(t, object{"ev": "call", "target": callMint})
	mint.until(t, hasWS("svg", "call-decline"))
	plum.json(t, object{"ev": "call", "target": callMint})
	plum.until(t, hasWS("svg", "Mint is busy"))
	tapCallControl(t, mint, "call-decline")
	cake.until(t, hasWS("call", `"reason":"declined"`))
	mint.until(t, hasWS("call", `"reason":"declined"`))
	checkCallStates(t, b.ws, callNone, callNone)

	b.ws.mu.Lock()
	b.ws.ringCall(b.ws.devices[callCake], callMint, 40*time.Millisecond)
	b.ws.mu.Unlock()
	cake.until(t, hasWS("call", `"reason":"no answer"`))
	mint.until(t, hasWS("call", `"reason":"no answer"`))
	checkCallStates(t, b.ws, callNone, callNone)

	cake.json(t, object{"ev": "call", "target": callMint})
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-answer")
	cake.until(t, hasWS("call", `"state":"start"`))
	mint.conn.Close()
	cake.until(t, hasWS("call", `"reason":"disconnected"`))
	checkCallStates(t, b.ws, callNone, callNone)

	old := testConnectWS(t, base, "unit", false)
	old.hello(t, "echoear-legacy", "Legacy")
	cake.json(t, object{"ev": "call", "target": "echoear-legacy"})
	cake.until(t, hasWS("svg", "firmware"))
	cake.json(t, object{"ev": "call", "target": "offline"})
	cake.until(t, hasWS("svg", "offline is offline"))
}

func TestWSCallCardAndVoiceCommand(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"transcript":"Call Mint!"}`)
		})
	})
	cake := callClient(t, b, base, callCake, "Cake")
	mint := callClient(t, b, base, callMint, "Mint")
	cake.json(t, object{"ev": "rec_start"})
	cake.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, make([]byte, callPCMBytes)...)))
	cake.json(t, object{"ev": "rec_end"})
	mint.until(t, hasWS("svg", "call-answer"))
	tapCallControl(t, mint, "call-decline")
	mint.until(t, hasWS("call", `"state":"end"`))
	checkCallStates(t, b.ws, callNone, callNone)

	cake.json(t, object{"ev": "intercom", "target": callMint, "text": "Hello Mint"})
	msg := mint.until(t, hasWS("svg", `data-id=\"call\"`))
	for _, control := range []string{"reply", "done"} {
		if !hasWS("svg", fmt.Sprintf(`data-id=\"%s\"`, control))(msg) {
			t.Fatalf("card is missing %q", control)
		}
	}
	tapCallControl(t, mint, "call")
	cake.until(t, hasWS("svg", "call-answer"))
	checkCallStates(t, b.ws, callRingingIn, callRingingOut)
}

func TestCallPCMBackpressure(t *testing.T) {
	h := &wsHub{devices: make(map[string]*wsDevice)}
	makeSide := func(id, peer string) *wsDevice {
		d := &wsDevice{key: id, callPeer: peer, callID: 1, callState: callActive,
			client: &wsClient{ctx: context.Background(), out: make(chan wsFrame, 8)}}
		h.devices[id] = d
		return d
	}
	caller := makeSide(callCake, callMint)
	callee := makeSide(callMint, callCake)
	for i := range 10 {
		h.relayCallPCM(caller, []byte{micPrefix, byte(i), 0})
	}
	if len(callee.client.out) != 6 {
		t.Fatal("call relay did not enforce its queue ceiling")
	}
	for i := range 6 {
		if got := <-callee.client.out; !bytes.Equal(got.data, []byte{pcmPrefix, byte(i), 0}) {
			t.Fatalf("unexpected PCM: %v", got)
		}
	}
}

func TestWSPagedVoiceMenu(t *testing.T) {
	b, base := wsHarness(t, nil)
	cake := callClient(t, b, base, callCake, "Cake")
	tapCallControl(t, cake, "menu")
	home := cake.until(t, hasWS("svg", "menu-call"))
	if !hasWS("svg", "menu-intercom")(home) {
		t.Fatal("main menu must offer both Call and Voice Note")
	}
	tapCallControl(t, cake, "menu-voice")
	for i, voice := range voiceCatalog {
		if i%tilesPerPage != 0 {
			continue
		}
		page := cake.until(t, hasWS("svg", "voice-"+strings.ToLower(voice.Name)))
		for _, want := range voiceCatalog[i:min(i+tilesPerPage, len(voiceCatalog))] {
			if !hasWS("svg", "voice-"+strings.ToLower(want.Name))(page) {
				t.Fatalf("missing voice %s from page %d", want.Name, i/tilesPerPage)
			}
		}
		if i+tilesPerPage < len(voiceCatalog) {
			tapCallControl(t, cake, "menu-next")
		}
	}
	tapCallControl(t, cake, "menu-prev")
	cake.until(t, hasWS("svg", "voice-"+strings.ToLower(voiceCatalog[tilesPerPage].Name)))
}

func TestHelloCallCapabilities(t *testing.T) {
	for _, tc := range []struct {
		event wsEvent
		board string
		want  deviceCaps
	}{
		{wsEvent{Role: "device"}, "esp-vocat", deviceCaps{Battery: true, HeadPads: true, Drag: true}},
		{wsEvent{Role: "device"}, "esp-box-3", deviceCaps{Drag: true}},
		{wsEvent{Role: "web", Caps: wsCaps{Call: new(true), Drag: new(true)}}, "", deviceCaps{Call: true, Drag: true}},
		{wsEvent{Role: "device", Caps: wsCaps{Battery: new(false), HeadPads: new(false), Call: new(true), Drag: new(false)}},
			"esp-vocat", deviceCaps{Call: true}},
	} {
		if got := helloCaps(tc.event, tc.board); got != tc.want {
			t.Errorf("%s %s: got %+v; want %+v", tc.event.Role, tc.board, got, tc.want)
		}
	}
}
