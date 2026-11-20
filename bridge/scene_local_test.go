package main

import (
	"bytes"
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
	for _, end := range []string{"exit", "replace", "state", "disconnect", "cancel", "reset"} {
		t.Run(end, func(t *testing.T) {
			b, base := wsHarness(t, nil)
			const id = "echoear-delay"
			c := testConnectWS(t, base, "unit", false)
			c.hello(t, id, "Mint")
			toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
				localButton("flash", object{"tap": object{"after_ms": 250, "then": object{"patch": localSet("caption", "Late")}}}),
				localButton("next", object{"tap": object{"state": "new"}}),
				{"id": "caption", "t": "text", "s": "Before"},
			}, "states": object{"new": object{"nodes": []object{{"id": "caption", "t": "text", "s": "Switched"}}}}}, false)
			c.json(t, object{"ev": "tap", "id": "flash", "x": 180, "y": 120})
			var d *wsDevice
			eventually(t, func() bool {
				b.ws.mu.Lock()
				defer b.ws.mu.Unlock()
				d = b.ws.devices[id]
				return len(d.sceneTimers) == 1
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
			case "cancel":
				b.ws.mu.Lock()
				b.ws.cancelTurn(d)
				b.ws.mu.Unlock()
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
			if nodeIn(d.scene, "caption")["s"] == "Late" || len(d.sceneTimers) != 0 {
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
		{"cycle": []object{{}}},
		{"tap": object{"patch": object{"expected_rev": 10}}},
	} {
		toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{localButton("bad", reaction)}}, true)
	}
}
