package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	imagecolor "image/color"
	"image/draw"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func portraitFixture(t *testing.T) []byte {
	t.Helper()
	bg, _ := directPaint("#FBEFE6")
	fur, _ := directPaint("#D4B5DE")
	ink, _ := directPaint("#583E62")
	img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)
	for i, offset := range []image.Point{{13, -21}, {-20, 15}, {8, 19}, {-15, -12}} {
		base := image.Pt(i%2*512, i/2*512)
		center := base.Add(image.Pt(256, 256)).Add(offset)
		for y := -160; y <= 160; y++ {
			for x := -165; x <= 165; x++ {
				if x*x*160*160+y*y*165*165 > 165*165*160*160 {
					continue
				}
				img.SetRGBA(center.X+x, center.Y+y, fur)
			}
		}
		body := []image.Rectangle{image.Rect(-30, 43, 30, 51), image.Rect(-36, -28, 36, -21),
			image.Rect(-33, 35, 33, 68), image.Rect(-25, 25, 20, 31)}[i].Add(center)
		draw.Draw(img, body, image.NewUniform(ink), image.Point{}, draw.Src)
		img.SetRGBA(base.X+22, base.Y+20, ink)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestPortraitAlignsTranslatedSilhouettes(t *testing.T) {
	raw := portraitFixture(t)
	sheet, err := decodePortraitSheet(raw)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := directPaint("#FFF2E7")
	poses, err := slicePortrait(sheet, p)
	if err != nil {
		t.Fatal(err)
	}
	var painted image.Rectangle
	for i, im := range poses {
		b := portraitBounds(im, im.Bounds(), p)
		if i == 0 {
			painted = b
		}
		center := b.Min.Add(b.Max)
		if b != painted || center.X < 359 || center.X > 361 || center.Y < 359 || center.Y > 361 {
			t.Fatalf("%s drifted: got %v, idle %v", portraitPoses[i], b, painted)
		}
		if i != 0 && bytes.Equal(im.Pix, poses[0].Pix) {
			t.Fatalf("%s did not preserve its expression", portraitPoses[i])
		}
	}
	if _, err := slicePortrait(image.NewUniform(imagecolor.White), p); err == nil {
		t.Fatal("accepted wrong sheet dimensions")
	}
	if _, err := slicePortrait(image.NewRGBA(image.Rect(0, 0, 1024, 1024)), p); err == nil {
		t.Fatal("accepted an empty sheet")
	}
}

func TestPortraitConsoleContract(t *testing.T) {
	source := []byte("reference fixture")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req object
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "bad fixture request", 400)
			return
		}
		config := req["generationConfig"].(object)
		parts := req["contents"].([]any)[0].(object)["parts"].([]any)
		asset := parts[1].(object)["inlineData"].(object)
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture-only" ||
			config["imageConfig"].(object)["imageSize"] != "1K" ||
			asset["mimeType"] != "image/png" || len(parts) != 2 {
			t.Error("invalid Console contract")
		}
		_ = json.NewEncoder(w).Encode(object{"candidates": []object{{"content": object{
			"parts": []object{{"inlineData": object{"mimeType": "image/png", "data": source}}},
		}}}})
	}))
	defer server.Close()
	got, err := requestPortrait(t.Context(), server.Client(), server.URL, "fixture-only", "A cozy owl", source)
	if err != nil || !bytes.Equal(got, source) {
		t.Fatalf("invalid Console response: %v", err)
	}
}

func TestPortraitPatchScopeAndNativeFrames(t *testing.T) {
	sheet := portraitFixture(t)
	dir := t.TempDir()
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.virtual = virtualStore(b.db)
	})
	own := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{}, 201))
	other := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{}, 201))
	id := str(own["id"])
	patch := func(spec object, failed bool) object {
		t.Helper()
		out := toolTest(t, base, "patch", object{"device_id": id, "cat": object{"portrait": spec, "save_profile": false}}, failed)
		cat, _ := out["cat"].(object)
		return cat
	}
	patch(object{"prompt": "draw an owl"}, true) // No configured generator or paid fallback.
	b.portraits = &portraitStore{dir: dir, generate: func(_ context.Context, prompt string, input []byte) ([]byte, error) {
		if strings.Contains(prompt, "add a scarf") != (len(input) > 0) {
			t.Error("edit source does not match the requested operation")
		}
		if len(input) > 0 {
			if _, err := png.Decode(bytes.NewReader(input)); err != nil {
				t.Error("sent an invalid reference sheet")
			}
		}
		return sheet, nil
	}}
	patch(object{"prompt": " ", "edit": true}, true)
	patch(object{"prompt": "invalid", "typo": true}, true)
	c := patch(object{"prompt": "draw an owl", "bg": "#eaf3fc"}, false)
	raw := str(c["face_svg"])
	asset := activePortrait(raw)
	if !portraitIDPattern.MatchString(asset) || len(raw) > 1600 || c["face_bg"] != "#EAF3FC" {
		t.Fatal(c)
	}
	for _, pose := range append(slices.Clone(portraitPoses[:]), "sheet") {
		fi, err := os.Stat(filepath.Join(dir, asset, pose+".png"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s not private: %v", pose, err)
		}
	}
	s1 := httptest.NewServer(b.scoped(virtualClient{ID: id}, "on-device"))
	defer s1.Close()
	s2 := httptest.NewServer(b.scoped(virtualClient{ID: str(other["id"])}, "other-device"))
	defer s2.Close()
	path := "/api/portraits/" + asset + "/idle.png"
	testRequest(t, "GET", s1.URL+path, "", nil, 401)
	testRequest(t, "GET", s2.URL+path, "other-device", nil, 404)
	testRequest(t, "GET", base+"/api/portraits/"+asset+"/sheet.png", "unit", nil, 404)
	out := testRequest(t, "GET", s1.URL+path, "on-device", nil, 200)
	if config, err := png.DecodeConfig(bytes.NewReader(out)); err != nil || config.Width != 360 || config.Height != 360 {
		t.Fatalf("expected aligned pose PNG: %v", err)
	}
	for _, pair := range []struct {
		mood, pose string
		blink      bool
	}{{"idle", "idle", false}, {"idle", "blink", true}, {"speaking", "talk", false}, {"thinking", "think", false},
		{"listening", "idle", false}, {"sleepy", "blink", false}} {
		scene := catSVG(catState{FaceSVG: raw, catLook: catLook{FaceBG: "#EAF3FC"}}, catPose{mood: pair.mood, blink: pair.blink, battery: -1})
		ref := `href="portrait:` + asset + ":" + pair.pose + `"`
		if strings.Count(scene, `href="portrait:`) != 1 || !strings.Contains(scene, ref) {
			t.Fatalf("wrong asset for %q", pair.mood)
		}
		day, err := b.portraits.frames.buildGradedWithAssets(scene, false, roundScreen, gradeDay, b.portraits.image)
		if err != nil || day.fb[180*360+180] == day.fb[180*360+50] {
			t.Fatalf("portrait did not render: %v", err)
		}
		night, err := b.portraits.frames.buildGradedWithAssets(scene, false, roundScreen, gradeNight, b.portraits.image)
		if err != nil || night.grade != gradeNight || night.fb[180*360+180] == day.fb[180*360+180] {
			t.Fatalf("portrait was not graded: %v", err)
		}
	}
	edited := patch(object{"prompt": "add a scarf", "edit": true}, false)
	if newAsset := activePortrait(str(edited["face_svg"])); newAsset == "" || asset == newAsset || edited["face_bg"] != c["face_bg"] {
		t.Fatal(edited)
	}
	testRequest(t, "GET", s1.URL+path, "on-device", nil, 404)
	toolTest(t, base, "patch", object{"device_id": str(other["id"]), "cat": object{"face_svg": raw}}, true)
}
