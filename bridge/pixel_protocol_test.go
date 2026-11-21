package main

import (
	"slices"
	"strings"
	"testing"
)

func TestMCPCompanionSprite(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-pixel-test"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "")
	spec := object{"rows": defaultStarterCatSprite.Rows, "palette": object{"p": "#DC9944", "X": "#FFFFFF"}}
	patch := func(cat object, fail bool) object {
		return toolTest(t, base, "patch", object{"device_id": id, "cat": cat}, fail)
	}
	out := patch(object{"sprite": spec, "save_profile": false}, false)
	cat := out["cat"].(object)
	raw := str(cat["face_svg"])
	pixels := map[screenGeometry][][]uint16{}
	for _, pose := range pixelPoses {
		if !strings.Contains(raw, `data-pose="`+pose+`"`) {
			t.Fatal("missing pose", pose)
		}
		root, err := parseFaceSVG(raw)
		if err != nil {
			t.Fatal(err)
		}
		layer := svgStart + svgString(faceLayer(root, pose)) + "</svg>"
		if _, _, err = renderSVG(layer); err != nil {
			t.Fatal(err)
		}
		for _, g := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
			view, err := buildScreen(layer, false, g)
			if err != nil || len(view.fb) != g.width*g.height {
				t.Fatalf("%s on %v: %v", pose, g, err)
			}
			for _, previous := range pixels[g] {
				if slices.Equal(view.fb, previous) {
					t.Fatalf("%s on %v matches an earlier pose", pose, g)
				}
			}
			pixels[g] = append(pixels[g], view.fb)
		}
	}
	for _, bad := range []object{
		{"rows": []string{"kk", "k"}},
		{"rows": []string{"kk", "kk"}, "palette": object{"k": "#fff"}},
		{"rows": []string{"kk", "kk"}, "palette": object{".": "#FFFFFF"}},
		{"rows": []string{"kk", "kk"}, "blink": object{"24": "kk"}},
		{"rows": []string{"kk", "kk"}, "blink": object{"3": "kk"}},
		{"rows": []string{"kk", "kk"}, "typo": "no"},
	} {
		patch(object{"sprite": bad}, true)
	}
	patch(object{"sprite": spec, "face_svg": "<svg/>"}, true)
	patch(object{"op": "reset_blank", "sprite": spec}, true)
	if got := str(stateMe(t, base, id)["cat"].(object)["face_svg"]); got != raw {
		t.Fatal("invalid patch changed the persisted face")
	}
}

const screenTestSpec = `{
  "title":"Rain & shine", "badge":"% < >",
  "body":[
    {"type":"sprite","id":"spark","rows":[".y.","yyy",".y."],"scale":9},
    {"type":"text","text":"Try <this> & that, then try another word. This is a longer line to exercise Go's font wrapping.","muted":true},
    {"type":"meter","label":"Energy","value":2.5,"max":3,"color":"l"},
    {"type":"grid","cols":2,"items":[{"id":"a","label":"A","sprite":["kk","kk"],"sub":"Active","active":true},{"id":"b","label":"B"}]}
  ],
  "actions":[{"id":"start","label":"Start","variant":"primary"}]
}`

func TestMCPDeclarativeScreen(t *testing.T) {
	_, base := wsHarness(t, nil)
	round := testConnectWS(t, base, "unit", false)
	box := testConnectWS(t, base, "unit", false)
	round.helloV2(t, "echoear-pixel-round", "")
	box.json(t, object{"ev": "hello", "role": "device", "device_id": "echoear-pixel-box", "proto": 2,
		"firmware": "0.9.5", "width": 320, "height": 240, "shape": "rect", "board": "esp-box-3"})
	box.until(t, isDisplay)
	ids := []string{"echoear-pixel-round", "echoear-pixel-box"}
	spec, err := decodeObject([]byte(screenTestSpec))
	if err != nil {
		t.Fatal(err)
	}
	out := toolTest(t, base, "app", object{"devices": ids, "screen": spec, "save_as": "rain-shine"}, false)
	if out["title"] != "Rain & shine" || out["saved"] == nil || out["layout_warnings"] != nil {
		t.Fatal(out)
	}
	if r, _, _ := decodeDisplay(t, round.until(t, isDisplay)); r.Dx() > 360 || r.Dy() > 360 {
		t.Fatal(r)
	}
	if r, _, _ := decodeDisplay(t, box.until(t, isDisplay)); r.Max.X > 320 || r.Max.Y > 240 {
		t.Fatal(r)
	}
	reloaded := toolTest(t, base, "app", object{"devices": ids, "load": "rain-shine"}, false)
	for _, id := range []string{exitNode, "start", "a", "b", "spark"} {
		if !slices.Contains(reloaded["hit_ids"].([]any), any(id)) {
			t.Fatal("missing reloaded control", id, reloaded)
		}
	}
	for _, bad := range []string{
		`{"body":[{"type":"text","text":"ok","id":"typo"}]}`,
		`{"body":[{"type":"grid","items":[{"id":"a","label":"A","typo":false}]}]}`,
		`{"body":[{"type":"meter","label":"bad","value":2,"max":0}]}`,
		`{"body":[],"actions":[{"id":"lark-exit","label":"Bad"}]}`,
		`{"body":[],"actions":[{"id":"echoear-exit","label":"Bad"}]}`,
		`{"body":[{"type":"sprite","rows":["ww","ww"],"id":"a"}],"actions":[{"id":"a","label":"Dup"}]}`,
		`{"body":[{"type":"sprite","rows":["ww","ww"],"palette":{"bad":"#112233"}}]}`,
	} {
		fields, err := decodeObject([]byte(bad))
		if err != nil {
			t.Fatal(err)
		}
		toolTest(t, base, "app", object{"devices": ids, "screen": fields}, true)
	}
}

func TestJSDeclarativeScreen(t *testing.T) {
	code := `function render() {
		if (!Object.isFrozen(ui.palette) || Object.keys(ui.palette).length !== 16) throw Error("palette");
		ui.palette.p = "lime";
		if (ui.palette.p !== "#F5B895") throw Error("changed palette");
		const sprite = ui.sprite(["XX",".X"], {x:3,y:6,scale:3,palette:{X:"#010203"}});
		if (!sprite.includes('width="6"') || !sprite.includes('fill="#010203"')) throw Error("sprite");
		for (const id of ["lark-exit","echoear-exit"]) {
			try { ui.choices({choices:[{id,label:"no"}]}); throw Error("accepted reserved id"); }
			catch (e) { if (!e.message.includes("reserved")) throw e; }
		}
		return ui.screen(` + screenTestSpec + `);
	}`
	for _, g := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		d := &wsDevice{screen: g, client: &wsClient{role: "device"}}
		spec := savedApp{Code: code}
		_, views, err := initApp(spec, []appPlayer{{id: "pixel"}}, nil, []*wsDevice{d}, nil, buildScreen)
		if err != nil {
			t.Fatal(err)
		}
		v := views[0]
		if len(v.fb) != g.width*g.height || len(appLayoutWarnings(views, g.shape == "round")) != 0 ||
			len(v.hits) != 5 || hitID(v.hits, 180, 320) != exitNode ||
			!strings.Contains(v.raw, `&lt;this&gt; &amp;`) {
			t.Fatalf("%v: %v, %v, %s", g, v.hits, appLayoutWarnings(views, g.shape == "round"), v.raw)
		}
		for _, h := range v.hits {
			if !isExitNode(h.id) && h.bounds.y2 > 290 {
				t.Fatalf("control too low %+v", h)
			}
		}
	}
}
