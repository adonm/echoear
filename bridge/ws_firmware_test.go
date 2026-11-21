package main

import "testing"

func TestNativeTapRequiresCurrentFrameRevision(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "echoear-rev"
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake",
		"caps": object{"pointer_rev": true}})
	_, rev, _ := decodeDisplay(t, d.until(t, isDisplay))

	tap := object{"ev": "tap", "x": 180, "y": 150}
	d.json(t, tap) // A client that advertised pointer_rev may not omit it.
	tap["rev_lo"] = rev - 1
	d.json(t, tap)
	d.barrier(t)
	b.ws.mu.Lock()
	started := !b.ws.devices[id].recAt.IsZero()
	rev = b.ws.devices[id].nativeRev // An idle blink may have produced another revision.
	b.ws.mu.Unlock()
	if started {
		t.Fatal("missing or stale revision started recording")
	}
	tap["rev_lo"] = rev
	d.json(t, tap)
	d.until(t, hasWS("rec", `"action":"start"`))
}

func TestDestructiveNativeRevisionWrapAndLegacy(t *testing.T) {
	d := wsDevice{caps: deviceCaps{PointerRev: true}}
	for _, action := range []string{"stop", "rec-cancel", "rec-send", "listen", "reply", "call-peer",
		"call-answer", "call-decline", "call-hangup", "call-mute"} {
		wrapped, old := uint16(0), uint16(65535)
		if !d.acceptPointerRevision(action, &wrapped) || d.acceptPointerRevision(action, &old) {
			t.Fatalf("%s accepts the wrong revision at wrap", action)
		}
	}
	d.caps.PointerRev = false
	if !d.acceptPointerRevision("stop", nil) {
		t.Fatal("legacy firmware must continue to work without rev_lo")
	}
}

func TestNativeWakeWordUsesTalkFlowOnlyWhenEnabled(t *testing.T) {
	b, base := wsHarness(t, nil)
	d := testConnectWS(t, base, "unit", false)
	const id = "echoear-wake"
	d.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake",
		"caps": object{"wake_word": true, "bridge_endpoint": true}})
	d.until(t, hasWS("hello_ok", ""))
	d.until(t, isDisplay)
	d.json(t, object{"ev": "wake_word"})
	d.barrier(t)
	b.ws.mu.Lock()
	dev := b.ws.devices[id]
	started := !dev.recAt.IsZero()
	endpoint := dev.caps.BridgeEndpoint
	b.ws.mu.Unlock()
	if started || !endpoint {
		t.Fatal("wake was not default-off or bridge endpoint capability was lost")
	}

	operator := testConnectWS(t, base, "unit", true)
	operator.json(t, object{"ev": "hello", "role": "web"})
	operator.json(t, object{"ev": "wake_word", "device_id": id, "enabled": true})
	operator.until(t, hasWS("wake_word", `"enabled":true`))
	d.until(t, hasWS("wake_word", `"enabled":true`))
	d.json(t, object{"ev": "wake_word"})
	d.until(t, hasWS("rec", `"action":"start"`))
	d.json(t, object{"ev": "rec_start"})
	d.barrier(t)
	b.ws.mu.Lock()
	open := dev.recOpen
	b.ws.mu.Unlock()
	if !open {
		t.Fatal("recording did not wait for the normal rec_start acknowledgement")
	}
}
