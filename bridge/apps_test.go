package main

import (
	"context"
	"encoding/json"
	"image"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestAppVMLeavesHubUnlockedAndCannotPublishAfterExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := &bridge{root: t.Context()}
		c := &wsClient{role: "web", ctx: t.Context(), out: make(chan wsFrame, 8)}
		d := &wsDevice{client: c, key: "phone-a100", screen: roundScreen, cat: blankCat()}
		h := &wsHub{b: b, devices: map[string]*wsDevice{d.key: d}}
		const code = `function init() { return {color: "red"}; }
			function render(s) { return '<svg><rect width="360" height="360" fill="' + s.color + '"/></svg>'; }
			function onTap(s) { waitForTest(); s.color = "lime"; chime("tap"); }`
		a, views, err := initApp(savedApp{Code: code}, []appPlayer{{id: d.key}}, nil, []*wsDevice{d})
		if err != nil {
			t.Fatal(err)
		}
		entered, proceed := make(chan struct{}), make(chan struct{})
		release := syncOnceClose(proceed)
		defer release()
		if err := a.vm.Set("waitForTest", func() {
			close(entered)
			<-proceed
		}); err != nil {
			t.Fatal(err)
		}
		a.ctx, a.cancel = context.WithCancel(t.Context())
		h.mu.Lock()
		d.app, d.appView = a, views[0]
		h.render(d, time.Now(), false)
		h.mu.Unlock()
		synctest.Wait()
		h.mu.Lock()
		h.updateApp(a, 0, "", 180, 180, time.Now())
		h.mu.Unlock()
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Fatal("app tap did not run")
		}
		if !h.mu.TryLock() {
			t.Fatal("VM held the global hub lock")
		}
		h.closeApp(a, time.Now())
		h.mu.Unlock()
		release()
		synctest.Wait()
		if d.app != nil || d.displayApp != nil || !strings.Contains(d.svg.source, "cat-ring") {
			t.Fatal("exited app published its superseded render")
		}
		for len(c.out) > 0 {
			f := <-c.out
			if hasWS("chime", `"kind":"tap"`)(f) || hasWS("svg", "lime")(f) {
				t.Fatal("exited app's effects escaped", string(f.data))
			}
		}
	})
}

func (s *testWS) helloV2(t *testing.T, id, cat string) wsFrame {
	t.Helper()
	s.json(t, object{"ev": "hello", "role": "device", "device_id": id, "character": cat, "firmware": "0.8.0", "proto": 2})
	return s.until(t, isDisplay)
}

func isDisplay(f wsFrame) bool { return f.op == wsBinary && len(f.data) > 0 && f.data[0] == 0x03 }

func TestAppSandboxAndErrors(t *testing.T) {
	players := []appPlayer{{"echoear-test", "Cake", 0}}
	for _, tc := range []struct{ code, want string }{
		{`while (true) {}`, "timeout"},
		{`function init(){for(;;){}} function render(){return "<svg/>"}`, "timeout"},
		{`app={get render(){for(;;){}}}`, "timeout"},
		{`function render(){ throw new Error("broken paint") }`, "broken paint"},
		{`function render(){ throw {toString(){for(;;){}}} }`, "JS exception"},
		{"\nfunction render(){ \n throw new Error('broken line');\n}", "app.js:3:"},
		{`function render(){return require("fs")}`, "require is not defined"},
	} {
		start := time.Now()
		_, _, err := initApp(savedApp{Code: tc.code}, players, nil, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("want %q, got %v", tc.want, err)
		}
		if tc.want == "timeout" && (time.Since(start) < 40*time.Millisecond || time.Since(start) > time.Second) {
			t.Fatalf("timeout took %v", time.Since(start))
		}
	}
	code := `function render() {
		if ([typeof require,typeof process,typeof fetch,typeof setTimeout,typeof readFile].some(x=>x!=="undefined")) throw "host leak";
		return '<svg/>';
	} function onTap(){while(true){}}`
	app, _, err := initApp(savedApp{Code: code}, players, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.update(0, "spot", 180, 180, nil); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatal(err)
	}
	if _, err := app.screens(nil); err != nil {
		t.Fatalf("interruption leaked into next call: %v", err)
	}
}

func TestWhackAMonsterOnV2Board(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "echoear-monster"
	device := testConnectWS(t, base, "unit", false)
	first := device.helloV2(t, id, "Cake")
	r, _, _ := decodeDisplay(t, first)
	if r != image.Rect(0, 0, 360, 360) {
		t.Fatalf("first frame not full: %v", r)
	}
	web := testConnectWS(t, base, "unit", true)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "cat-ring"))
	const code = `app={
	init(ctx){ return {n:0,x:100}; },
	render(s,p){ return '<svg viewBox="0 0 360 360"><rect width="360" height="360" fill="black"/><circle data-id="monster" cx="'+s.x+'" cy="180" r="30" fill="lime"/><text x="180" y="100" fill="white">Score '+s.n+'</text></svg>'; },
	onTap(s,p,id){ if(id==="monster"){ s.n++; storeSet("score",s.n); if(storeGet("score")!==s.n)throw "store did not read its write"; play("C5:40 E5:40"); } },
	onTick(s){ s.x+=50; return s; }
}`
	out := toolTest(t, base, "run_app", object{"devices": id, "title": "Whack a Monster", "code": code, "save_as": "monster", "tick_ms": 250, "max_ticks": 1}, false)
	if out["saved_as"] != "monster" || !slices.Contains(out["hit_ids"].([]any), "monster") {
		t.Fatal(out)
	}
	f := device.until(t, isDisplay)
	decodeDisplay(t, f)
	web.until(t, hasWS("svg", "Score 0"))
	web.until(t, hasWS("svg", `cx=\"150\"`))
	// Trust hardware coordinates, not an ID claimed by a client.
	device.json(t, object{"ev": "tap", "x": 150, "y": 180, "id": "not-the-monster"})
	device.until(t, func(f wsFrame) bool { return f.op == wsBinary && len(f.data) > 1 && f.data[0] == pcmPrefix })
	web.until(t, hasWS("svg", "Score 1"))
	a := toolTest(t, base, "inspect", object{"device_id": id}, false)["active_app"].(object)
	if a["ticks_left"] != json.Number("0") {
		t.Fatal(a)
	}
	mem := toolTest(t, base, "store", object{"op": "get", "scope": "app:monster", "key": "score"}, false)
	if mem["value"] != json.Number("1") {
		t.Fatal(mem)
	}
	device.json(t, object{"ev": "tap", "x": 180, "y": 318})
	web.until(t, hasWS("svg", "cat-ring"))
	if a := toolTest(t, base, "inspect", object{"device_id": id}, false)["active_app"]; a != nil {
		t.Fatal(a)
	}
	if app, err := b.loadApp(t.Context(), "monster"); err != nil || app.Code != code || app.TickMS != 250 || app.MaxTicks != 1 {
		t.Fatalf("saved %+v (%v)", app, err)
	}
	testRequest(t, "POST", base+"/api/apps/run", "unit", object{"device_id": id, "name": "monster"}, 200)
	testRequest(t, "DELETE", base+"/api/apps/monster", "unit", nil, 204)
	if apps := testObject(t, testRequest(t, "GET", base+"/api/apps", "unit", nil, 200))["apps"].([]any); len(apps) != 0 {
		t.Fatal(apps)
	}
}

const ticTacToe = `
function init(ctx) { if (ctx.players.length!==2) throw new Error("need two players"); return { turn:0, cells:Array(9).fill(-1), winner:-1 }; }
function onTap(s,p,id) {
  if (!/^cell[0-8]$/.test(id) || p.index!==s.turn || s.winner!==-1) return;
  const i=+id.slice(4); if (s.cells[i]!==-1) return;
  s.cells[i]=p.index;
  for (const [a,b,c] of [[0,1,2],[3,4,5],[6,7,8],[0,3,6],[1,4,7],[2,5,8],[0,4,8],[2,4,6]]) {
    if (s.cells[a]===p.index && s.cells[b]===p.index && s.cells[c]===p.index) {
      s.winner=p.index; playAll(["G5:40"]); return s;
    }
  }
  s.turn=1-s.turn;
}
function render(s,p) {
  const label=s.winner===p.index?"You win!":s.winner!==-1?"Other player won":s.turn===p.index?"Your turn!":"Waiting...";
  let svg='<svg viewBox="0 0 360 360"><text x="180" y="85" fill="#fff" text-anchor="middle">'+label+'</text>';
  for (let i=0;i<9;i++) {
    const x=90+(i%3)*60, y=115+Math.floor(i/3)*60;
    const c=["#ff0000","#0000ff"][s.cells[i]] || "#cccccc";
    svg+='<rect data-id="cell'+i+'" x="'+x+'" y="'+y+'" width="52" height="52" rx="6" fill="'+c+'"/>';
  }
  return svg+'</svg>';
}`

func TestTicTacToePersonalizedAndAuthoritative(t *testing.T) {
	a, views, err := initApp(savedApp{Code: ticTacToe}, []appPlayer{{"a", "Cake", 0}, {"b", "Mint", 1}}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireView := func(index int, text string) {
		t.Helper()
		if !strings.Contains(views[index].source, text) {
			t.Fatal("missing personalized text:", text)
		}
	}
	requireView(0, "Your turn!")
	requireView(1, "Waiting...")
	apply := func(index int, id string) {
		t.Helper()
		views, err = a.update(index, id, 150, 150, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	apply(1, "cell2")
	requireView(0, "Your turn!")
	for i, next := range []string{"cell0", "cell3", "cell1", "cell4", "cell2"} {
		apply(i%2, next)
	}
	requireView(0, "You win!")
	requireView(1, "Other player won")
	if len(a.effects) != 1 || !a.effects[0].all || len(a.effects[0].pcm) == 0 {
		t.Fatal("winner did not play all")
	}
	apply(1, "cell5")
	requireView(0, "You win!")
}

func TestSVGDirtyFrameMatchesBrowserAndExit(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-rect"
	board, web := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", true)
	first, _, initial := decodeDisplay(t, board.helloV2(t, id, "Cake"))
	if first != image.Rect(0, 0, 360, 360) {
		t.Fatal(first)
	}
	web.json(t, object{"ev": "hello", "role": "web"})
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "cat-ring"))
	const svg = `<svg viewBox="0 0 360 360"><rect width="360" height="360" fill="#122234"/><rect data-id="tile" x="179" y="179" width="2" height="2" fill="#123456"/></svg>`
	run := func(raw string) {
		t.Helper()
		toolTest(t, base, "run_app", object{"devices": id, "svg": raw}, false)
		wire := web.until(t, hasWS("svg", "tile"))
		o := testObject(t, wire.data)
		rect, rev, p := decodeDisplay(t, board.until(t, isDisplay))
		if o["rev"] != json.Number("2") && o["rev"] != json.Number("3") {
			t.Fatal(o["rev"])
		}
		if raw != svg && rect != image.Rect(179, 179, 181, 181) {
			t.Fatalf("dirty bounds %v, rev=%d", rect, rev)
		}
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			copy(initial[y*360+rect.Min.X:], p[(y-rect.Min.Y)*rect.Dx():(y-rect.Min.Y+1)*rect.Dx()])
		}
		fb, _, err := renderSVG(str(o["svg"]))
		if err != nil || !slices.Equal(initial, fb[:]) {
			t.Fatalf("browser SVG and device framebuffer disagree: %v", err)
		}
	}
	run(svg)
	run(strings.Replace(svg, "#123456", "#654321", 1))
	if out := toolTest(t, base, "run_app", object{"devices": id, "code": "function render(){ throw Error('wrong value') }"}, true); !strings.Contains(str(out["error"]), "app.js:1") {
		t.Fatal(out)
	}
	snap := toolTest(t, base, "inspect", object{"device_id": id}, false)
	if snap["active_app"] == nil {
		t.Fatal("failed validation changed active app")
	}
}
