package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryCRUDAndAtomicCAS(t *testing.T) {
	b, base := wsHarness(t, nil)
	const scope = "game:tictactoe"
	args := object{"op": "set", "scope": scope, "key": "round:1", "value": object{"move": 0}, "expected_version": 0}
	if set := toolTest(t, base, "memory", args, false); set["version"] != json.Number("1") {
		t.Fatal(set)
	}
	if conflict := toolTest(t, base, "memory", args, true); conflict["error"] != "version_conflict" || conflict["version"] != json.Number("1") {
		t.Fatal(conflict)
	}
	a, z := make(chan error, 2), make(chan struct{})
	var workers sync.WaitGroup
	for i := 1; i <= 2; i++ {
		workers.Go(func() {
			<-z
			expected := uint64(1)
			_, err := b.db.setMemory(t.Context(), memoryWrite{scope, "round:1", fmt.Sprintf(`{"move":%d}`, i)}, &expected)
			a <- err
		})
	}
	close(z)
	workers.Wait()
	close(a)
	wins, conflicts := 0, 0
	for err := range a {
		var conflict *versionConflict
		switch {
		case err == nil:
			wins++
		case errors.As(err, &conflict) && conflict.version == 2:
			conflicts++
		default:
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
	if got := toolTest(t, base, "memory", object{"op": "get", "scope": scope, "key": "round:1"}, false); got["found"] != true || got["version"] != json.Number("2") {
		t.Fatal(got)
	}
	list := toolTest(t, base, "memory", object{"op": "list", "scope": scope}, false)["entries"].([]any)
	if len(list) != 1 || list[0].(object)["value"] != nil || list[0].(object)["key"] != "round:1" {
		t.Fatal(list)
	}
	api := base + "/api/memory"
	list = testObject(t, testRequest(t, "GET", api+"?scope="+scope, "unit", nil, 200))["entries"].([]any)
	if len(list) != 1 || list[0].(object)["value"].(object)["move"] == json.Number("0") {
		t.Fatal(list)
	}
	scopes := apiObjects(t, api+"/scopes", "scopes")
	if len(scopes) != 1 || scopes[0].(object)["scope"] != scope {
		t.Fatal(scopes)
	}
	testRequest(t, "DELETE", api+"?scope="+scope+"&key=round:1", "unit", nil, 204)
	if got := toolTest(t, base, "memory", object{"op": "get", "scope": scope, "key": "round:1"}, false); got["found"] != false {
		t.Fatal(got)
	}
	args["scope"] = "game:Bad"
	toolTest(t, base, "memory", args, true)
	args["scope"], args["value"] = "shared", strings.Repeat("x", 16384)
	toolTest(t, base, "memory", args, true)
	for i := range 64 {
		if _, err := b.db.setMemory(t.Context(), memoryWrite{"shared", fmt.Sprint(i), "null"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.db.setMemory(t.Context(), memoryWrite{"shared", "extra", "null"}, nil); err == nil {
		t.Fatal("accepted 65th key")
	}
	if n, err := b.db.setMemory(t.Context(), memoryWrite{"shared", "0", "true"}, nil); err != nil || n != 2 {
		t.Fatal("cannot update full scope", n, err)
	}
	if got := toolTest(t, base, "memory", object{"op": "delete", "scope": "shared", "key": "0"}, false); got["deleted"] != true {
		t.Fatal(got)
	}
}

func TestAssetCRUDAndSceneLoad(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-board"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	save := object{"op": "save", "name": "board", "author": "Cake", "bg": "#122334", "title": "Start",
		"nodes":  []object{{"id": "next", "t": "btn", "x": -10, "y": 100, "w": 90, "h": 30, "tap": object{"state": "done"}}},
		"states": object{"done": object{"nodes": []object{{"id": "done", "t": "text", "s": "Won!", "x": 450}}}}}
	if result := toolTest(t, base, "asset", save, false); result["node_count"] != json.Number("1") {
		t.Fatal(result)
	}
	got := toolTest(t, base, "asset", object{"op": "get", "name": "board"}, false)
	data := got["data"].(object)
	if got["author"] != "Cake" || data["nodes"].([]any)[0].(object)["x"] != json.Number("0") ||
		data["states"].(object)["done"].(object)["nodes"].([]any)[0].(object)["x"] != json.Number("359") {
		t.Fatal(got)
	}
	toolTest(t, base, "scene", object{"device_id": id, "asset": "board", "save_as": "board-copy", "bg": "#000000",
		"nodes": []object{{"id": "extra", "t": "text", "s": "Extra"}}}, false)
	c.until(t, hasWS("scene", `"id":"extra"`))
	c.json(t, object{"ev": "tap", "id": "next", "x": 10, "y": 110})
	c.until(t, hasWS("scene", "Won!"))
	list := toolTest(t, base, "asset", object{"op": "list"}, false)["assets"].([]any)
	if len(list) != 2 || list[1].(object)["name"] != "board-copy" {
		t.Fatal(list)
	}
	copy := toolTest(t, base, "asset", object{"op": "get", "name": "board-copy"}, false)["data"].(object)
	if copy["bg"] != "#000000" || len(copy["nodes"].([]any)) != 2 {
		t.Fatal(copy)
	}
	testRequest(t, "DELETE", base+"/api/assets/board", "unit", nil, 204)
	if got := toolTest(t, base, "asset", object{"op": "delete", "name": "board-copy"}, false); got["deleted"] != true {
		t.Fatal(got)
	}
	if got := toolTest(t, base, "asset", object{"op": "get", "name": "board"}, false); got["found"] != false {
		t.Fatal(got)
	}
}

func TestRoomBroadcastBetweenWebSockets(t *testing.T) {
	_, base := wsHarness(t, nil)
	const a, b = "echoear-room1", "echoear-room2"
	one, two := testConnectWS(t, base, "unit", false), testConnectWS(t, base, "unit", false)
	one.hello(t, a, "Cake")
	two.hello(t, b, "Mint")
	for _, id := range []string{a, b} {
		toolTest(t, base, "scene", object{"device_id": id, "room": "race", "nodes": []object{
			{"id": "piece", "t": "circle", "x": 80, "y": 120, "r": 12},
			{"id": "move", "t": "btn", "x": 135, "y": 190, "w": 70, "h": 32,
				"cycle": []object{
					{"broadcast": true, "chime": "tap", "move": object{"target": "piece", "dx": 20, "dy": 0},
						"patch": object{"set": object{"piece": object{"c": "#112233"}}},
						"store": object{"scope": "game:race", "key": "turn", "value": "mint"}},
					{"broadcast": true, "chime": "think", "move": object{"target": "piece", "dx": -20, "dy": 0},
						"store": object{"scope": "game:race", "key": "turn", "value": "cake"}},
				}},
		}}, false)
	}
	one.until(t, hasWS("scene", `"id":"move"`))
	two.until(t, hasWS("scene", `"id":"move"`))
	devices := toolTest(t, base, "devices", object{}, false)["devices"].([]any)
	if len(devices) != 2 || devices[0].(object)["room"] != "race" || devices[1].(object)["room"] != "race" {
		t.Fatal(devices)
	}
	move := object{"ev": "tap", "id": "move", "x": 150, "y": 200}
	start := time.Now()
	one.json(t, move)
	for _, conn := range []*testWS{one, two} {
		conn.until(t, hasWS("chime", `"kind":"tap"`))
		patch := conn.until(t, hasWS("patch", `"x":100`))
		if !bytes.Contains(patch.data, []byte(`"c":"#112233"`)) {
			t.Fatalf("missing patch: %s", patch.data)
		}
	}
	elapsed := time.Since(start)
	t.Logf("room patch + chime queued and received by both sockets in %v", elapsed)
	if elapsed >= 5*time.Millisecond {
		t.Errorf("room reaction latency %v exceeds 5 ms", elapsed)
	}
	// The next player advances the same cycle rather than replaying step one.
	two.json(t, move)
	for _, conn := range []*testWS{one, two} {
		conn.until(t, hasWS("chime", `"kind":"think"`))
		conn.until(t, hasWS("patch", `"x":80`))
	}
	if got := toolTest(t, base, "memory", object{"op": "get", "scope": "game:race", "key": "turn"}, false); got["value"] != "cake" || got["version"] != json.Number("2") {
		t.Fatal("tap didn't persist exactly once per move", got)
	}
	toolTest(t, base, "scene", object{"device_id": a, "preset": "face"}, false)
	if result := toolTest(t, base, "snapshot", object{"device_id": a}, false); result["device"].(object)["room"] != "" {
		t.Fatal("cat face left room occupied", result)
	}
}
