package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMemoryCRUDAndAtomicCAS(t *testing.T) {
	b, base := wsHarness(t, nil)
	const scope = "game:tictactoe"
	args := object{"op": "set", "scope": scope, "key": "round:1", "value": object{"move": 0}, "expected_version": 0}
	if set := toolTest(t, base, "store", args, false); set["version"] != json.Number("1") {
		t.Fatal(set)
	}
	if conflict := toolTest(t, base, "store", args, true); conflict["error"] != "version_conflict" || conflict["version"] != json.Number("1") {
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
	if got := toolTest(t, base, "store", object{"op": "get", "scope": scope, "key": "round:1"}, false); got["found"] != true || got["version"] != json.Number("2") {
		t.Fatal(got)
	}
	list := toolTest(t, base, "store", object{"op": "list", "scope": scope}, false)["entries"].([]any)
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
	if got := toolTest(t, base, "store", object{"op": "get", "scope": scope, "key": "round:1"}, false); got["found"] != false {
		t.Fatal(got)
	}
	args["scope"] = "game:Bad"
	toolTest(t, base, "store", args, true)
	args["scope"], args["value"] = "shared", strings.Repeat("x", 16384)
	toolTest(t, base, "store", args, true)
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
	if got := toolTest(t, base, "store", object{"op": "delete", "scope": "shared", "key": "0"}, false); got["deleted"] != true {
		t.Fatal(got)
	}
}
