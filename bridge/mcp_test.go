package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRequest(t *testing.T, method, url, token string, data any, want int) []byte {
	t.Helper()
	var in io.Reader
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(raw)
	}
	r, err := http.NewRequestWithContext(t.Context(), method, url, in)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", jsonType)
	r.Header.Set("X-EchoEar-Token", token)
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s: HTTP %d, want %d (%s; %v)", r.URL.Path, res.StatusCode, want, raw, err)
	}
	return raw
}

func testObject(t *testing.T, raw []byte) object {
	t.Helper()
	value, err := decodeObject(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return value
}

func rpcTest(t *testing.T, base, method string, params object) object {
	t.Helper()
	raw := testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "id": "test", "method": method, "params": params}, 200)
	value := testObject(t, raw)
	if value["id"] != "test" || value["jsonrpc"] != "2.0" {
		t.Fatalf("invalid JSON-RPC: %s", raw)
	}
	result, _ := value["result"].(object)
	if result == nil {
		t.Fatalf("RPC error: %s", raw)
	}
	return result
}

func toolTest(t *testing.T, base, tool string, args object, failed bool) object {
	t.Helper()
	result := rpcTest(t, base, "tools/call", object{"name": tool, "arguments": args})
	value, _ := result["structuredContent"].(object)
	if value == nil || result["isError"] != failed {
		t.Fatalf("%s: %+v (expected error: %v)", tool, result, failed)
	}
	return value
}

func fakeTTS(ctx context.Context, requests *atomic.Int32) *synthWorker {
	w := &synthWorker{requests: make(chan ttsRequest), done: make(chan struct{})}
	w.ready.Store(true)
	go func() {
		defer close(w.done)
		for {
			select {
			case <-ctx.Done():
				return
			case req := <-w.requests:
				if requests != nil {
					requests.Add(1)
				}
				req.done <- ttsResult{pcm: []byte{1, 2, 3, 4}}
			}
		}
	}()
	return w
}

func TestMCPProtocol(t *testing.T) {
	_, base := wsHarness(t, nil)
	const id = "echoear-test"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	testRequest(t, "GET", base+"/mcp", "unit", nil, 405)
	testRequest(t, "DELETE", base+"/mcp", "unit", nil, 204)
	testRequest(t, "POST", base+"/mcp", "", object{}, 401)
	testRequest(t, "POST", base+"/mcp", "unit", []string{"bad"}, 400)
	init := rpcTest(t, base, "initialize", object{"protocolVersion": "2025-06-18"})
	if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(object)["name"] != "echoear" {
		t.Fatal(init)
	}
	testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "method": "notifications/initialized"}, 202)
	if len(rpcTest(t, base, "ping", object{})) != 0 {
		t.Fatal("ping should return an empty result")
	}
	names := []string{}
	for _, raw := range rpcTest(t, base, "tools/list", object{})["tools"].([]any) {
		tool := raw.(object)
		names = append(names, str(tool["name"]))
		if tool["inputSchema"].(object)["type"] != "object" {
			t.Fatal(tool)
		}
	}
	if !slices.Equal(names, []string{"inspect", "run_app", "act", "store"}) {
		t.Fatal(names)
	}
	old := testObject(t, testRequest(t, "POST", base+"/mcp", "unit", object{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": object{"name": "speak"}}, 200))
	if old["error"].(object)["code"] != json.Number("-32602") {
		t.Fatal(old)
	}
	snap := toolTest(t, base, "inspect", object{"device_id": id}, false)
	if len(snap) != 9 || snap["me"].(object)["name"] != "Cake" || snap["screen"].(object)["title"] != "Cat" {
		t.Fatal(snap)
	}
	toolTest(t, base, "act", object{"to": id, "volume": 40}, false)
	toolTest(t, base, "store", object{"op": "set", "scope": "app:scores", "key": "best", "value": 7, "expected_version": 0}, false)
	if v := toolTest(t, base, "store", object{"op": "delete", "scope": "app:scores", "key": "best", "expected_version": 0}, true); v["version"] != json.Number("1") {
		t.Fatal(v)
	}
}

func TestMCPControl(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "echoear-control"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	watcher := testConnectWS(t, base, "unit", false)
	watcher.json(t, object{"ev": "hello", "role": "web"})
	watcher.until(t, hasWS("peers", id))
	watcher.json(t, object{"ev": "watch", "device_id": id})
	watcher.until(t, hasWS("power", `"brightness":85`))

	for _, invalid := range []object{
		{"to": id}, {"to": id, "volume": -1}, {"to": id, "volume": 101},
		{"to": id, "volume": 1.5}, {"to": id, "sleep": "true"},
	} {
		toolTest(t, base, "act", invalid, true)
	}
	b.ws.mu.Lock()
	d := b.ws.devices[id]
	b.ws.startTurn(d, time.Now(), false, func(context.Context, uint64) {})
	turn := d.turn
	b.ws.mu.Unlock()
	out := toolTest(t, base, "act", object{"to": id, "volume": 0, "sleep": true}, false)
	if out["ok"] != true || out["to"] != id || out["volume"] != json.Number("0") || out["sleeping"] != true {
		t.Fatal(out)
	}
	for _, client := range []*testWS{c, watcher} {
		client.until(t, hasWS("volume", `"level":0`))
		client.until(t, hasWS("power", `"brightness":12`))
	}
	b.ws.finishTurn(d, turn, nil)
	checkStatus := func(mode, volume string) {
		t.Helper()
		device := toolTest(t, base, "inspect", object{"device_id": id}, false)["me"].(object)
		if device["mode"] != mode || device["volume"] != json.Number(volume) {
			t.Fatal(device)
		}
	}
	checkStatus("sleepy", "0")
	if wake := toolTest(t, base, "act", object{"to": id, "sleep": false}, false); wake["sleeping"] != false || wake["volume"] != json.Number("0") {
		t.Fatal(wake)
	}
	c.until(t, hasWS("power", `"brightness":85`))
	if vol := toolTest(t, base, "act", object{"to": id, "volume": 42}, false); vol["sleeping"] != false || vol["volume"] != json.Number("42") {
		t.Fatal(vol)
	}
	c.until(t, hasWS("volume", `"level":42`))
	checkStatus("idle", "42")
}

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(time.Millisecond) {
		if f() {
			return
		}
	}
	t.Fatal("condition did not become true")
}

func chatFrame(text string) func(wsFrame) bool {
	return func(f wsFrame) bool {
		return f.op == wsText && bytes.Contains(f.data, []byte(`"type":"chat"`)) && bytes.Contains(f.data, []byte(text))
	}
}

func TestMCPStdioForwards(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer unit" {
			t.Error("incorrect proxy request")
		}
		posts.Add(1)
		var req rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if len(req.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		_, _ = fmt.Fprintf(w, "{\n\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"method\":%q}\n}", req.ID, req.Method)
	}))
	defer s.Close()
	t.Setenv("ECHOEAR_BRIDGE_TOKEN", "")
	t.Setenv("ECHOEAR_BRIDGE_PORT", "")
	file := filepath.Join(t.TempDir(), "bridge.env")
	port := s.Listener.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(file, []byte("ECHOEAR_BRIDGE_TOKEN='unit'\nECHOEAR_BRIDGE_PORT="+strconv.Itoa(port)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	url, token, err := mcpProxyConfig(file)
	if err != nil || url != s.URL+"/mcp" || token != "unit" {
		t.Fatal("proxy could not load its env file")
	}
	var out bytes.Buffer
	in := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n" +
		"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n{\"jsonrpc\":\"2.0\",\"id\":\"p\",\"method\":\"ping\"}\n")
	if err := proxyMCP(t.Context(), s.Client(), url, token, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if posts.Load() != 3 || len(lines) != 2 {
		t.Fatal("proxy did not forward all messages / suppressed notification response")
	}
	got := map[string]bool{}
	for _, line := range lines {
		value := testObject(t, []byte(line))
		got[str(value["result"].(object)["method"])] = true
	}
	if !got["initialize"] || !got["ping"] {
		t.Fatal(out.String())
	}
}
