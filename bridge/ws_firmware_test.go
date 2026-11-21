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
