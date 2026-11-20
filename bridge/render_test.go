package main

import (
	"encoding/binary"
	"image"
	"slices"
	"strings"
	"testing"
)

func TestRasterSVGShapesTextAndHitOrder(t *testing.T) {
	raw := svgStart + `<rect width="360" height="360" fill="#101820"/>
<defs><linearGradient id="rainbow"><stop offset="0" stop-color="red"/><stop offset="1" stop-color="blue"/></linearGradient></defs>
<rect data-id="behind" x="110" y="80" width="140" height="100" fill="url(#rainbow)"/>
<circle data-id="orb" cx="180" cy="120" r="20" fill="lime"/>
<ellipse data-id="ellipse" cx="180" cy="192" rx="18" ry="10" fill="red"/>
<polygon data-id="triangle" points="78,166 102,166 90,190" fill="#ff0"/>
<line x1="90" y1="220" x2="120" y2="220" stroke="#fff" stroke-width="4"/>
<polyline points="100,240 120,250 140,240" fill="none" stroke="#fff" stroke-width="3"/>
<path d="M200 198 Q225 235 250 198 T300 198 L300 245 L200 245 Z" fill="#00f"/>
<g transform="translate(100,0)" data-id="group" data-hit="31,262,115,25">
<text x="80" y="281" fill="#fff" font-size="22" text-anchor="middle" font-weight="bold" data-id="label">Go Font</text>
</g></svg>`
	fb, hits, err := renderSVG(raw)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[image.Point]uint16{
		{0, 0}: 0, {180, 120}: 0x07e0, {180, 192}: 0xf800, {90, 173}: 0xffe0, {96, 220}: 0xffff, {230, 236}: 0x001f,
	} {
		if got := fb[p.Y*360+p.X]; got != want {
			t.Errorf("pixel %v = %04x, want %04x", p, got, want)
		}
	}
	if fb[100*360+118] == fb[100*360+242] {
		t.Fatal("gradient painted flat")
	}
	if hitID(hits, 157, 120) != "orb" || hitID(hits, 130, 105) != "behind" || hitID(hits, 180, 190) != "ellipse" ||
		hitID(hits, 90, 172) != "triangle" || hitID(hits, 180, 274) != "label" || hitID(hits, 245, 291) != "group" {
		t.Fatal("wrong z-order, padding or hit geometry:", hits)
	}
	var textPixels int
	for y := 260; y < 283; y++ {
		for x := 130; x < 229; x++ {
			if fb[y*360+x] == 0xffff {
				textPixels++
			}
		}
	}
	if textPixels < 30 {
		t.Fatal("font outline is missing")
	}
}

func TestSVGSanitization(t *testing.T) {
	input := `<?xml version="1.0"?><!DOCTYPE svg><svg xmlns:xlink="https://evil.example" viewBox="0 0 360 360" onload="bad()">
<script>bad()</script><foreignObject><b>bad</b></foreignObject><image href="https://evil.example"/>
<a href="javascript:bad()"><text x="180" y="180">bad</text></a><style>bad</style>
<rect x="150" y="150" width="60" height="60" data-id="ok" onclick="bad()" style="bad" fill="url(https://evil.example)"/>
</svg>`
	got, hits, err := prepareSVG(input, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"onload", "onclick", "style", "DOCTYPE", "href", "evil", "foreignObject", "<script", "javascript:"} {
		if strings.Contains(got, bad) {
			t.Errorf("unsafe %q survived: %s", bad, got)
		}
	}
	if hitID(hits, 180, 180) != "ok" || hitID(hits, 180, 318) != exitNode {
		t.Fatal(hits)
	}
	for _, raw := range []string{`<svg><rect></svg>`, `<svg/><svg/>`, `<svg viewBox="0 0 10 10"/>`} {
		if _, _, err := renderSVG(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, _, err := renderSVG(got); err != nil {
		t.Fatal(err)
	}
}

// Interpret the wire with the firmware contract, independently of the encoder.
func decodeDisplay(t *testing.T, f wsFrame) (image.Rectangle, uint16, []uint16) {
	t.Helper()
	p := f.data
	if f.op != wsBinary || len(p) < 12 || p[0] != 0x03 {
		t.Fatalf("not a display packet: %x", p[:min(len(p), 12)])
	}
	u := func(i int) int { return int(binary.LittleEndian.Uint16(p[i:])) }
	x, y, w, h := u(1), u(3), u(5), u(7)
	if w < 1 || h < 1 || x+w > 360 || y+h > 360 {
		t.Fatalf("invalid rect %d %d %d %d", x, y, w, h)
	}
	c, rev, raw := p[9], uint16(u(10)), p[12:]
	out := make([]uint16, 0, w*h)
	switch c {
	case 0:
		for len(raw) >= 2 {
			out = append(out, binary.BigEndian.Uint16(raw))
			raw = raw[2:]
		}
	case 1:
		for len(raw) > 0 {
			tag := raw[0]
			raw = raw[1:]
			n := int(tag&127) + 1
			if tag&0x80 != 0 {
				if len(raw) < 2 {
					t.Fatal("short repeat run")
				}
				for range n {
					out = append(out, binary.BigEndian.Uint16(raw))
				}
				raw = raw[2:]
			} else {
				if len(raw) < n*2 {
					t.Fatal("short literal run")
				}
				for range n {
					out = append(out, binary.BigEndian.Uint16(raw))
					raw = raw[2:]
				}
			}
			if len(out) > w*h {
				t.Fatal("run overflows dirty rectangle")
			}
		}
	default:
		t.Fatalf("unknown codec %d", c)
	}
	if len(out) != w*h || len(raw) != 0 {
		t.Fatal("malformed display payload")
	}
	return image.Rect(x, y, x+w, y+h), rev, out
}

func TestDirtyRectAndRLE565(t *testing.T) {
	var previous, next framebuffer
	rect := image.Rect(18, 20, 280, 22)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			p := uint16(0xf800)
			if y == 21 && x > 140 {
				p = 0x001f
			}
			next[y*360+x] = p
		}
	}
	if r := dirtyRect(&previous, &next); r != rect {
		t.Fatal(r)
	}
	if r := dirtyRect(&next, &next); !r.Empty() {
		t.Fatal(r)
	}
	if r := dirtyRect(nil, &next); r != image.Rect(0, 0, 360, 360) {
		t.Fatal(r)
	}
	frame := displayFrame(&next, rect, 0x10303)
	if got := frame.data[:12]; !slices.Equal(got, []byte{3, 18, 0, 20, 0, 6, 1, 2, 0, 1, 3, 3}) {
		t.Fatalf("header %x", got)
	}
	r, rev, p := decodeDisplay(t, frame)
	if r != rect || rev != 0x303 || !slices.Equal(p[:262], next[20*360+18:20*360+280]) ||
		!slices.Equal(p[262:], next[21*360+18:21*360+280]) {
		t.Fatal("RLE round trip changed pixels")
	}
	for i := range 129 {
		next[360+i] = uint16(i)*213 + 1
	}
	frame = displayFrame(&next, image.Rect(0, 1, 129, 2), 1)
	_, _, p = decodeDisplay(t, frame)
	if frame.data[9] != 0 || !slices.Equal(p, next[360:489]) || frame.data[12] != 0 || frame.data[13] != 1 {
		t.Fatal("incompressible pixels did not use raw big-endian fallback")
	}
	// Mix literals and repeats long enough to require multiple run tags.
	for i := range 300 {
		if i < 130 {
			next[i] = uint16(i)
		} else {
			next[i] = 0x1234
		}
	}
	frame = displayFrame(&next, image.Rect(0, 0, 300, 1), 19)
	_, _, p = decodeDisplay(t, frame)
	if frame.data[9] != 1 || !slices.Equal(p, next[:300]) {
		t.Fatal("mixed runs did not round-trip")
	}
}
