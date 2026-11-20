package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func localButton(id string, controls object) object {
	node := object{"id": id, "t": "rect", "x": 100, "y": 100, "w": 160, "h": 48}
	for key, v := range controls {
		node[key] = v
	}
	return node
}

func localSet(id, text string) object { return object{"set": object{id: object{"s": text}}} }

func TestSceneTriangleProtocol(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-tri"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	fin := object{"id": "fin", "t": "tri", "x1": 180, "y1": 120, "x2": 150, "y2": 180, "x3": 210, "y3": 180,
		"c": "#38bdf8", "sw": 3, "tap": object{"chime": "tap"}}
	toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{fin}}, false)
	frame := testObject(t, c.until(t, hasWS("scene", `"id":"fin"`)).data)
	want := testObject(t, []byte(`{"id":"fin","t":"tri","x1":180,"y1":120,"x2":150,"y2":180,"x3":210,"y3":180,"c":"#38BDF8","fill":true,"sw":3,"on":"tap"}`))
	if got := frame["nodes"].([]any)[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong triangle wire fields: %v", got)
	}
	// Near the third vertex, outside the nominal triangle, but inside the padded box.
	c.json(t, object{"ev": "tap", "x": 218, "y": 189})
	c.until(t, hasWS("chime", `"kind":"tap"`))
	c.json(t, object{"ev": "tap", "x": 230, "y": 189})
	c.json(t, object{"ev": "status", "volume": 73}) // Process the tap before inspecting the event ring.
	c.until(t, hasWS("volume", `"level":73`))
	snapshot := toolTest(t, base, "snapshot", object{"device_id": id}, false)
	events := snapshot["events"].([]any)
	if len(events) != 2 || events[0].(object)["data"].(object)["id"] != "fin" || events[1].(object)["data"].(object)["id"] != "" {
		t.Fatalf("wrong triangle hit bounds: %v", events)
	}
	toolTest(t, base, "patch", object{"device_id": id, "set": object{
		"fin": object{"x3": 400, "y3": -100, "fill": false},
	}}, false)
	got := testObject(t, c.until(t, hasWS("patch", "")).data)["set"]
	if want := testObject(t, []byte(`{"fin":{"x3":359,"y3":0,"fill":false}}`)); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong triangle patch: %v", got)
	}
}

func TestLocalGroupMoveRevertsOverWebSocket(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-group"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Mint")
	toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		{"id": "body", "t": "circle", "group": "fish", "x": 330, "y": 120, "r": 8, "c": "#38BDF8"},
		{"id": "tail", "t": "line", "x1": 110, "y1": 18, "x2": 120, "y2": 45},
		{"id": "fish", "t": "text", "x": 140, "y": 80, "s": "Fish"},
		{"id": "other", "t": "text", "x": 140, "y": 80, "s": "Still"},
		localButton("swim", object{"cycle": []object{
			{"move": object{"target": "fish", "dx": 40, "dy": -30}, "revert_ms": 80,
				"patch": object{"bg": "#123456", "set": object{"body": object{"c": "#ABCDEF"}}}},
			{"move": object{"target": "tail", "dx": -10, "dy": 0}},
		}}),
	}}, false)
	if first := c.until(t, hasWS("scene", `"id":"body"`)); bytes.Contains(first.data, []byte(`"group"`)) {
		t.Fatalf("scene leaked group: %s", first.data)
	}
	toolTest(t, base, "patch", object{"device_id": id, "set": object{"tail": object{"group": "fish"}}}, false)
	if meta := c.until(t, hasWS("patch", "")); bytes.Contains(meta.data, []byte(`"group"`)) {
		t.Fatalf("patch leaked group: %s", meta.data)
	}
	toolTest(t, base, "patch", object{"device_id": id, "add": []object{
		{"id": "fin", "t": "tri", "group": "fish", "x1": 150, "y1": 60, "x2": 130, "y2": 100, "x3": 170, "y3": 80},
	}}, false)
	frame := c.until(t, func(f wsFrame) bool {
		return hasWS("scene", `"id":"fin"`)(f) || hasWS("patch", `"id":"fin"`)(f)
	})
	if bytes.Contains(frame.data, []byte(`"group"`)) {
		t.Fatalf("added node leaked group: %s", frame.data)
	}
	rev, _ := testObject(t, frame.data)["rev"].(json.Number).Int64()
	tap := func() { c.json(t, object{"ev": "tap", "id": "swim"}) }
	tap()
	for _, expected := range []struct{ bg, set string }{
		{"#123456", `{"body":{"x":359,"y":90,"c":"#ABCDEF"},"tail":{"x1":150,"y1":0,"x2":160,"y2":15},"fish":{"x":180,"y":50},"fin":{"x1":190,"y1":30,"x2":170,"y2":70,"x3":210,"y3":50}}`},
		{"#0B1020", `{"body":{"x":330,"y":120,"c":"#38BDF8"},"tail":{"x1":110,"y1":18,"x2":120,"y2":45},"fish":{"x":140,"y":80},"fin":{"x1":150,"y1":60,"x2":130,"y2":100,"x3":170,"y3":80}}`},
	} {
		got := testObject(t, c.until(t, hasWS("patch", "")).data)
		rev++
		gotRev, _ := got["rev"].(json.Number).Int64()
		if got["bg"] != expected.bg || gotRev != rev ||
			!reflect.DeepEqual(got["set"], testObject(t, []byte(expected.set))) ||
			len(got["add"].([]any))+len(got["del"].([]any)) != 0 {
			t.Fatalf("wrong motion patch at rev %d: %v", rev, got)
		}
	}
	tap()
	got := testObject(t, c.until(t, hasWS("patch", "")).data)["set"]
	if want := testObject(t, []byte(`{"tail":{"x1":100,"x2":110}}`)); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrong ID-targeted motion: %v", got)
	}
}

func TestLocalTapMovesFishOverWebSocket(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-fish"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	move := func(y int) object { return object{"set": object{"fish": object{"y": y}}} }
	toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		{"id": "fish", "t": "circle", "x": 180, "y": 210, "r": 16, "fill": true,
			"tap": object{"patch": move(120), "after_ms": 80, "then": object{"patch": move(210)}}},
	}}, false)
	initial := testObject(t, c.until(t, hasWS("scene", `"id":"fish"`)).data)
	c.json(t, object{"ev": "tap", "id": "fish", "x": 180, "y": 210})
	rev := initial["rev"].(json.Number)
	for _, y := range []json.Number{"120", "210"} {
		frame := testObject(t, c.until(t, hasWS("patch", "")).data)
		next := frame["rev"].(json.Number)
		oldRev, _ := rev.Int64()
		newRev, _ := next.Int64()
		set, _ := frame["set"].(object)
		fish, _ := set["fish"].(object)
		if fish["y"] != y || newRev != oldRev+1 {
			t.Fatalf("want fish.y=%s at rev %d, got %v", y, oldRev+1, frame)
		}
		rev = next
	}
}

func TestLocalPatchRejectsMissingTargets(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-no-target"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	set := object{"missing": object{"y": 120}}
	scene := toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		localButton("bounce", object{"tap": object{"patch": object{"set": set}}}),
	}}, false)
	c.until(t, hasWS("scene", `"id":"bounce"`))
	if result := toolTest(t, base, "patch", object{"device_id": id, "set": set}, true); result["error"] != "Unknown node ID in set: missing" {
		t.Fatal(result)
	}
	c.json(t, object{"ev": "tap", "id": "bounce", "x": 180, "y": 120})
	c.until(t, hasWS("error", `"error":"Unknown node ID in set: missing"`))
	after := toolTest(t, base, "snapshot", object{"device_id": id}, false)["scene"].(object)
	if after["rev"] != scene["rev"] {
		t.Fatalf("rejected patch advanced revision: %v", after)
	}
}

func TestLocalNoOpPatchSendsCompleteFrame(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-no-op"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		localButton("bounce", object{"tap": object{"patch": object{"set": object{"bounce": object{"y": 100}}},
			"after_ms": 80, "then": object{"patch": object{"set": object{"bounce": object{"y": 120}}}}}}),
	}}, false)
	c.until(t, hasWS("scene", `"id":"bounce"`))
	c.json(t, object{"ev": "tap", "id": "bounce", "x": 180, "y": 120})
	first := testObject(t, c.until(t, hasWS("patch", "")).data)
	set, setOK := first["set"].(object)
	add, addOK := first["add"].([]any)
	del, delOK := first["del"].([]any)
	if !setOK || !addOK || !delOK || len(set)+len(add)+len(del) != 0 {
		t.Fatalf("expected a complete empty patch, got %v", first)
	}
	c.until(t, hasWS("patch", `"bounce":{"y":120}`))
}

func TestLocalSceneReactions(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) { b.worker = fakeTTS(b.root, nil) })
	const id = "echoear-local"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	watch := testConnectWS(t, base, "unit", false)
	watch.json(t, object{"ev": "hello", "role": "web"})
	watch.until(t, hasWS("peers", id))
	watch.json(t, object{"ev": "watch", "device_id": id})
	watch.until(t, hasWS("scene", ""))

	scene := toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		localButton("bounce", object{"tap": object{"chime": "celebrate", "speak": "Found you!",
			"patch": localSet("caption", "Peek"), "after_ms": 100,
			"then": object{"patch": localSet("caption", "Hidden"), "after_ms": 50, "then": object{"patch": localSet("caption", "Ready again")}}}}),
		{"id": "caption", "t": "text", "s": "Ready"},
		localButton("flip", object{"cycle": []object{
			{"patch": localSet("caption", "Red")}, {"patch": localSet("caption", "Blue")},
		}}),
		localButton("next", object{"tap": object{"state": "two", "patch": localSet("page2", "Second!")}}),
	}, "states": object{
		"two": object{"bg": "#123456", "title": "Two", "nodes": []object{
			{"id": "page2", "t": "text", "s": "Second"}, localButton("next", object{"tap": object{"state": "three"}})}},
		"three": object{"nodes": []object{localButton("done", object{"tap": object{"exit": true, "chime": "tap"}})}},
	}}, false)
	visual := c.until(t, hasWS("scene", `"id":"bounce"`))
	frame := testObject(t, visual.data)
	for _, raw := range frame["nodes"].([]any) {
		node := raw.(object)
		for _, k := range []string{"tap", "cycle", "action"} {
			if _, ok := node[k]; ok {
				t.Fatalf("server declaration %s leaked: %s", k, visual.data)
			}
		}
		if node["t"] == "rect" && node["on"] != "tap" {
			t.Fatalf("local action didn't auto-enable taps: %v", node)
		}
	}
	tap := func(node string) { c.json(t, object{"ev": "tap", "id": node, "x": 180, "y": 120}) }

	wait := make(chan object, 1)
	go func() {
		out, err := b.ws.waitEvent(t.Context(), id, nil, time.Second)
		if err != nil {
			t.Error(err)
		}
		wait <- out
	}()
	eventually(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.hasWaiter(b.ws.devices[id])
	})
	tap("bounce")
	if ev, ok := (<-wait)["event"].(deviceEvent); !ok || ev.Data["id"] != "bounce" {
		t.Fatalf("local control did not wake wait_event: %v", ev)
	}
	c.until(t, hasWS("chime", `"kind":"intercom"`))
	patched := c.until(t, hasWS("patch", `"s":"Peek"`))
	if mirror := watch.until(t, hasWS("patch", `"s":"Peek"`)); !bytes.Equal(mirror.data, patched.data) {
		t.Fatal("local patch did not mirror")
	}
	if testObject(t, patched.data)["rev"] == scene["rev"] {
		t.Fatal("local patch did not increment revision")
	}
	talked, hidden := false, false
	c.until(t, func(f wsFrame) bool {
		talked = talked || f.op == wsBinary && bytes.Equal(f.data, []byte{pcmPrefix, 1, 2, 3, 4})
		hidden = hidden || hasWS("patch", `"s":"Hidden"`)(f)
		return talked && hidden
	})
	c.until(t, hasWS("patch", `"s":"Ready again"`))
	c.json(t, object{"ev": "tts_ack", "state": "done"})

	tap("flip")
	c.until(t, hasWS("patch", `"s":"Red"`))
	toolTest(t, base, "patch", object{"device_id": id, "set": object{"flip": object{"c": "#FFFFFF"}}}, false)
	tap("flip")
	c.until(t, hasWS("patch", `"s":"Blue"`))
	tap("flip")
	c.until(t, hasWS("patch", `"s":"Red"`))

	toolTest(t, base, "patch", object{"device_id": id, "set": object{"flip": object{"cycle": []object{
		{"patch": localSet("caption", "Green")}, {"patch": localSet("caption", "Pink")},
	}}}, "add": []object{localButton("added", object{"tap": object{"patch": localSet("caption", "Added")}})}}, false)
	tap("flip")
	c.until(t, hasWS("patch", `"s":"Green"`))
	tap("added")
	c.until(t, hasWS("patch", `"s":"Added"`))

	tap("next")
	switched := c.until(t, hasWS("scene", `"id":"echoear-exit"`))
	if testObject(t, switched.data)["bg"] != "#123456" {
		t.Fatal("named state did not get its own background")
	}
	c.until(t, hasWS("patch", "Second!"))
	tap("next")
	c.until(t, hasWS("scene", `"id":"done"`))
	tap("done")
	c.until(t, hasWS("scene", `"id":"cat-ring"`))
}

func TestLocalSceneStopsTimers(t *testing.T) {
	for _, end := range []string{"exit", "replace", "state", "disconnect", "reset"} {
		t.Run(end, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			const id = "echoear-delay"
			c := testConnectWS(t, base, "unit", false)
			c.hello(t, id, "Mint")
			toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
				localButton("flash", object{"tap": object{"revert_ms": 250, "patch": localSet("caption", "Flash"),
					"after_ms": 250, "then": object{"patch": localSet("caption", "Late")}}}),
				localButton("next", object{"tap": object{"state": "new"}}),
				{"id": "caption", "t": "text", "s": "Before"},
			}, "states": object{"new": object{"nodes": []object{{"id": "caption", "t": "text", "s": "Switched"}}}}}, false)
			c.json(t, object{"ev": "tap", "id": "flash", "x": 180, "y": 120})
			var d *wsDevice
			eventually(t, func() bool {
				b.ws.mu.Lock()
				defer b.ws.mu.Unlock()
				d = b.ws.devices[id]
				return len(d.sceneTimers) == 2
			})
			switch end {
			case "exit":
				c.json(t, object{"ev": "tap", "id": exitNode})
				c.until(t, hasWS("scene", "cat-ring"))
			case "replace":
				toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{{"id": "caption", "t": "text", "s": "Replaced"}}}, false)
			case "state":
				c.json(t, object{"ev": "tap", "id": "next"})
				c.until(t, hasWS("scene", "Switched"))
			case "disconnect":
				c.conn.Close()
			case "reset":
				b.ws.cancelSceneUI(id)
			}
			eventually(t, func() bool {
				b.ws.mu.Lock()
				defer b.ws.mu.Unlock()
				return len(d.sceneTimers) == 0
			})
			time.Sleep(260 * time.Millisecond)
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			if text := nodeIn(d.scene, "caption")["s"]; text == "Late" || text == "Before" || len(d.sceneTimers) != 0 {
				t.Fatal("cancelled timer ran")
			}
		})
	}
}

func TestLocalSceneValidation(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-validate"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	for _, reaction := range []object{
		{"tap": object{"after_ms": 49, "then": object{}}},
		{"tap": object{"after_ms": 50}},
		{"tap": object{"after_ms": 50, "then": object{"after_ms": 50, "then": object{"after_ms": 50, "then": object{}}}}},
		{"tap": object{"state": "missing"}},
		{"tap": object{"revert_ms": 49}},
		{"tap": object{"revert_ms": 10001}},
		{"tap": object{"move": object{"target": "fish", "dx": 1}}},
		{"tap": object{"move": object{"target": "fish", "dx": 1, "dy": 0.5}}},
		{"cycle": []object{{}}},
		{"tap": object{"patch": object{"expected_rev": 10}}},
	} {
		toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{localButton("bad", reaction)}}, true)
	}
}
