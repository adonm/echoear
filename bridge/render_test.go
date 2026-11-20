package main

import (
	"encoding/binary"
	"image"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func facePoseSVGs() []string {
	cat := blankCat()
	return []string{
		catSVG(cat, catPose{mood: "idle", battery: 80}),
		catSVG(cat, catPose{mood: "idle", battery: 80, blink: true}),
		catSVG(cat, catPose{mood: "speaking", battery: 80}),
		catSVG(cat, catPose{mood: "sleepy", battery: 80}),
	}
}

func TestFacePoseCacheReusesFramesAndHitboxes(t *testing.T) {
	var cache screenCache
	poses := facePoseSVGs()
	prior := make([]svgScreen, len(poses))
	for i, svg := range poses {
		screen, err := cache.build(svg, false, roundScreen)
		if err != nil || len(screen.fb) != screenSize*screenSize || hitID(screen.hits, 180, 180) != "cat-ring" {
			t.Fatalf("pose %d did not build: %v", i, err)
		}
		prior[i] = screen
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for i, svg := range poses {
				screen, err := cache.build(svg, false, roundScreen)
				if err != nil || &screen.fb[0] != &prior[i].fb[0] || &screen.hits[0] != &prior[i].hits[0] {
					t.Errorf("pose %d did not reuse cached pixels and hits: %v", i, err)
				}
			}
		})
	}
	wg.Wait()
	if hits, misses := cache.stats(); hits != 4*uint64(len(poses)) || misses != uint64(len(poses)) {
		t.Fatalf("cache stats: hits=%d misses=%d", hits, misses)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		_, _ = cache.build(poses[0], false, roundScreen)
	}); allocs != 0 {
		t.Fatalf("cache hit allocated %.2f times", allocs)
	}
}

func TestScreenCacheSeparatesGeometryAndEvicts(t *testing.T) {
	var cache screenCache
	const raw = `<svg><rect width="360" height="360" fill="red"/></svg>`
	for _, geometry := range []screenGeometry{{}, {320, 240, "round"}, {320, 240, "rect"}, roundScreen} {
		screen, err := cache.build(raw, false, geometry)
		if err != nil || len(screen.fb) != geometry.width*geometry.height {
			t.Fatalf("geometry %+v: %v, %d pixels", geometry, err, len(screen.fb))
		}
		if geometry.width != 0 && (screen.fb[0] != 0) != (geometry.shape == "rect") {
			t.Fatalf("cache mixed screen shapes: %+v", geometry)
		}
	}
	withExit, err := cache.build(raw, true, screenGeometry{})
	if err != nil || hitID(withExit.hits, 180, 318) != exitNode || withExit.fb != nil {
		t.Fatal("web-only drawing did not retain its exit hitbox", err)
	}
	for i := range screenCacheSize {
		if _, err := cache.build(`<svg><text>`+strconv.Itoa(i)+`</text></svg>`, false, screenGeometry{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cache.build(raw, false, screenGeometry{}); err != nil {
		t.Fatal(err)
	}
	if hits, misses := cache.stats(); hits != 0 || misses != screenCacheSize+6 || len(cache.entries) != screenCacheSize {
		t.Fatalf("cache did not bound its FIFO: hits=%d misses=%d size=%d", hits, misses, len(cache.entries))
	}
}

func TestSVGSingleArgumentScalePreservesRasterHeight(t *testing.T) {
	const svg = `<svg><defs><linearGradient id="shade" gradientTransform="scale(.8333)"><stop stop-color="#FFFFFF"/></linearGradient></defs>
		<path d="M12 3v16" stroke="#FFFFFF" fill="none" stroke-width="2.6" transform="translate(100 100) scale(0.8333)"/></svg>`
	screen, err := buildScreenUncached(svg, false, roundScreen)
	if err != nil {
		t.Fatal(err)
	}
	height := 0
	for y := 100; y < 120; y++ {
		if slices.ContainsFunc(screen.fb[y*360+106:y*360+114], func(px uint16) bool { return px != 0 }) {
			height++
		}
	}
	if height <= 8 || !strings.Contains(screen.source, `gradientTransform="matrix(0.8333 0 0 0.8333 0 0)"`) {
		t.Fatalf("scaled icon occupies %d rows; gradient transform not normalized: %t", height, !strings.Contains(screen.source, `gradientTransform="matrix(0.8333 0 0 0.8333 0 0)"`))
	}
}

func BenchmarkFacePoseCache(b *testing.B) {
	poses := facePoseSVGs()
	for _, display := range []struct {
		name     string
		geometry screenGeometry
	}{{"360x360", roundScreen}, {"320x240", screenGeometry{320, 240, "rect"}}} {
		b.Run(display.name+"/uncached", func(b *testing.B) {
			b.ReportAllocs()
			for i := range b.N {
				if _, err := buildScreenUncached(poses[i%len(poses)], false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(display.name+"/hit", func(b *testing.B) {
			var cache screenCache
			for _, pose := range poses {
				if _, err := cache.build(pose, false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if _, err := cache.build(poses[i%len(poses)], false, display.geometry); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

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
	root, hits, err := prepareSVG(input, true)
	if err != nil {
		t.Fatal(err)
	}
	got := svgString(root)
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
	previous, next := make([]uint16, 360*360), make([]uint16, 360*360)
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
	if r := dirtyRect(previous, next, roundScreen); r != rect {
		t.Fatal(r)
	}
	if r := dirtyRect(next, next, roundScreen); !r.Empty() {
		t.Fatal(r)
	}
	if r := dirtyRect(nil, next, roundScreen); r != image.Rect(0, 0, 360, 360) {
		t.Fatal(r)
	}
	frame := displayFrame(next, rect, 0x10303, roundScreen.width)
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
	frame = displayFrame(next, image.Rect(0, 1, 129, 2), 1, roundScreen.width)
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
	frame = displayFrame(next, image.Rect(0, 0, 300, 1), 19, roundScreen.width)
	_, _, p = decodeDisplay(t, frame)
	if frame.data[9] != 1 || !slices.Equal(p, next[:300]) {
		t.Fatal("mixed runs did not round-trip")
	}
}

func TestRectangularScreen(t *testing.T) {
	geometry := screenGeometry{320, 240, "rect"}
	const bg = svgStart + `<rect width="360" height="360" fill="#123456"/>`
	blank, err := buildScreen(bg+`</svg>`, false, geometry)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range blank.fb {
		if p != 0x11aa {
			t.Fatalf("background pixel %d = %04x", i, p)
		}
	}
	art, err := buildScreen(bg+`<rect x="120" y="90" width="120" height="180" fill="red"/>
		<rect width="30" height="30" fill="lime" data-id="corner"/></svg>`, false, geometry)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[image.Point]uint16{
		{0, 0}: 0x11aa, {319, 239}: 0x11aa, {40, 0}: 0x07e0,
		{120, 60}: 0xf800, {199, 179}: 0xf800, {200, 179}: 0x11aa,
	} {
		if got := art.fb[p.Y*geometry.width+p.X]; got != want {
			t.Errorf("pixel %v = %04x, want %04x", p, got, want)
		}
	}
	if rect := dirtyRect(blank.fb, art.fb, geometry); rect != image.Rect(40, 0, 200, 180) {
		t.Fatalf("dirty rectangle = %v", rect)
	}
	full := dirtyRect(nil, art.fb, geometry)
	f := displayFrame(art.fb, full, 7, geometry.width)
	if got := f.data[:12]; !slices.Equal(got, []byte{3, 0, 0, 0, 0, 64, 1, 240, 0, 1, 7, 0}) {
		t.Fatalf("header %x", got)
	}
	if r, _, pixels := decodeDisplay(t, f); r != full || !slices.Equal(pixels, art.fb) {
		t.Fatal("native frame round trip changed pixels")
	}
	for p, want := range map[image.Point]image.Point{
		{0, 0}: {0, 0}, {40, 0}: {0, 0}, {42, 2}: {3, 3}, {160, 120}: {180, 180},
		{200, 150}: {240, 225}, {279, 239}: {358, 358}, {319, 239}: {359, 358},
	} {
		x, y := geometry.inverseTap(p.X, p.Y)
		if image.Pt(x, y) != want {
			t.Errorf("tap %v mapped to %d,%d, want %v", p, x, y, want)
		}
	}
}

func TestBox3HelloAndRawAppTaps(t *testing.T) {
	_, base := wsHarness(t, nil)
	box := testConnectWS(t, base, "unit", false)
	round := testConnectWS(t, base, "unit", false)
	web := testConnectWS(t, base, "unit", true)
	const boxID, roundID = "box3-b324", "echoear-round"
	box.json(t, object{"ev": "hello", "role": "device", "device_id": boxID, "proto": 2, "firmware": "0.8.0",
		"width": 320, "height": 240, "shape": "rect", "board": "esp-box-3"})
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); r != image.Rect(0, 0, 320, 240) {
		t.Fatalf("BOX-3 initial frame = %v", r)
	}
	if r, _, _ := decodeDisplay(t, round.helloV2(t, roundID, "Cake")); r != image.Rect(0, 0, 360, 360) {
		t.Fatalf("legacy hello initial frame = %v", r)
	}
	devices := testObject(t, testRequest(t, "GET", base+"/api/devices", "unit", nil, 200))["devices"].([]any)
	boxStatus := devices[0].(object)
	if boxStatus["device_id"] != boxID || boxStatus["board"] != "esp-box-3" ||
		boxStatus["character"] != "New Cat" || boxStatus["cat"].(object)["configured"] != false ||
		boxStatus["ws_connected"] != true {
		t.Fatal(boxStatus)
	}

	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "device_id": boxID})
	web.until(t, hasWS("svg", "cat-ring"))
	code := `function init(){ return {hits:"untouched"} }
	function render(s){ return '<svg><rect width="360" height="360" fill="#123456"/>'+
		'<rect width="30" height="30" fill="red" data-id="corner"/><text x="100" y="180">'+s.hits+'</text></svg>' }
	function onTap(s,p,id,x,y){ s.hits=id+":"+x+","+y; }`
	toolTest(t, base, "app", object{"devices": []string{boxID, roundID}, "code": code}, false)
	web.until(t, hasWS("svg", "untouched"))
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); !r.In(image.Rect(0, 0, 320, 240)) {
		t.Fatalf("BOX-3 app sent an out-of-bounds frame: %v", r)
	}
	box.json(t, object{"ev": "tap", "x": 48, "y": 10})
	web.until(t, hasWS("svg", "corner:12,15"))
	web.json(t, object{"ev": "tap", "x": 20, "y": 18})
	web.until(t, hasWS("svg", "corner:20,18"))
}
