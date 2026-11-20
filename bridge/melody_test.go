package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestSynthesizeMelody(t *testing.T) {
	notes := []string{"C5:120", "R:80", "F#4:120", "Bb4:120", "880:60"}
	pcm, err := synthesizeMelody(notes)
	if err != nil || len(pcm) != sampleRate {
		t.Fatalf("got %d bytes: %v", len(pcm), err)
	}
	endFirst := 120 * sampleRate / 500
	if binary.LittleEndian.Uint16(pcm) != 0 || binary.LittleEndian.Uint16(pcm[endFirst-2:]) != 0 ||
		!bytes.Equal(pcm[endFirst:200*sampleRate/500], make([]byte, 80*sampleRate/500)) {
		t.Fatal("note edges or rest are not silent")
	}
	peak := 0
	for sample := 0; sample < len(pcm); sample += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[sample:])))
		peak = max(peak, v, -v)
	}
	if peak < 11000 || peak > 15000 {
		t.Fatalf("peak %d out of range", peak)
	}
	for _, bad := range [][]string{nil, make([]string, 33), {"H4:100"}, {"D7:100"}, {"99:100"}, {"NaN:100"},
		{"C5:29"}, {"R:2001"}, {"R:2000", "R:2000", "R:2000", "R:30"}} {
		if _, err := synthesizeMelody(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestWSMelody(t *testing.T) {
	_, base := wsHarness(t, nil)
	id := "echoear-melody"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	notes := []string{"C5:120", "R:80"}
	pcm, _ := synthesizeMelody(notes)
	check := func() {
		t.Helper()
		frame := c.until(t, func(f wsFrame) bool { return f.op == wsBinary })
		if len(frame.data) == 0 || frame.data[0] != 0x02 || !bytes.Equal(frame.data[1:], pcm) {
			t.Fatalf("bad PCM frame (%d bytes)", len(frame.data))
		}
	}
	toolTest(t, base, "chime", object{"device_id": id}, true)
	toolTest(t, base, "chime", object{"device_id": id, "notes": notes}, false)
	check()
	toolTest(t, base, "scene", object{"device_id": id, "notes": notes, "nodes": []object{
		localButton("bell", object{"tap": object{"notes": notes}}),
	}}, false)
	c.until(t, hasWS("scene", `"id":"bell"`))
	check()
	c.json(t, object{"ev": "tap", "id": "bell"})
	check()
}
