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

func TestMCPProtocolAndScene(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.history = loadChatHistory(filepath.Join(dir, "chat_events.json"))
	})
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
	if !slices.Equal(names, []string{"devices", "snapshot", "scene", "patch", "speak", "chime", "intercom", "wait_event"}) {
		t.Fatal(names)
	}
	list := toolTest(t, base, "devices", object{}, false)
	d := list["devices"].([]any)[0].(object)
	if len(d) != len(mcpDeviceFields) || d["device_id"] != id || d["kind"] != "physical" || d["ws_connected"] != true {
		t.Fatal(d)
	}
	args := object{"device_id": id}
	face := toolTest(t, base, "snapshot", args, false)
	if scene := face["scene"].(object); scene["custom_scene"] != false || len(scene["nodes"].([]any)) < 10 {
		t.Fatal(scene)
	}
	create := toolTest(t, base, "scene", object{"device_id": id, "bg": "#000000", "title": "Quiz", "nodes": []object{
		{"id": "a", "t": "btn", "x": -4, "y": 500, "w": 90, "h": 35, "s": "Old", "ask": "Would launch a turn"},
		{"id": "b", "t": "text", "x": 180, "y": 200, "s": "Gone"},
	}}, false)
	c.until(t, hasWS("scene", `"id":"echoear-exit"`))
	snapshot := toolTest(t, base, "snapshot", args, false)["scene"].(object)
	node := snapshot["nodes"].([]any)[1].(object)
	if snapshot["custom_scene"] != true || node["x"] != json.Number("0") || node["y"] != json.Number("359") || node["on"] != "tap" {
		t.Fatal(snapshot)
	}
	expected := create["rev"]
	patched := toolTest(t, base, "patch", object{"device_id": id, "expected_rev": expected,
		"set": object{"a": object{"s": "New"}}, "del": []string{"b"}}, false)
	c.until(t, hasWS("patch", `"s":"New"`))
	if conflict := toolTest(t, base, "patch", object{"device_id": id, "expected_rev": expected}, true); conflict["rev"] != patched["rev"] {
		t.Fatal(conflict)
	}
	toolTest(t, base, "scene", object{"device_id": id, "preset": "face"}, false)
	if toolTest(t, base, "snapshot", args, false)["scene"].(object)["custom_scene"] != false {
		t.Fatal("face preset left custom scene set")
	}
	nodes := make([]object, maxNodes+4)
	for i := range nodes {
		nodes[i] = object{"id": fmt.Sprintf("n%d", i), "t": "text", "s": "x"}
	}
	toolTest(t, base, "scene", object{"device_id": id, "nodes": nodes}, false)
	snapshot = toolTest(t, base, "snapshot", args, false)["scene"].(object)
	if capped := snapshot["nodes"].([]any); len(capped) != maxNodes || capped[maxNodes-1].(object)["id"] != exitNode {
		t.Fatal("node cap did not preserve Cat exit")
	}
	messages := testObject(t, testRequest(t, "GET", base+"/api/devices/"+id+"/messages", "unit", nil, 200))["messages"].([]any)
	if first := messages[0].(object); first["scene_title"] != "Quiz" || first["source"] != "mcp" {
		t.Fatal(first)
	}
	entries := loadChatHistory(b.history.path)
	if len(entries.entries[id]) != 3 {
		t.Fatal(entries.entries[id])
	}
}

func TestMCPWaiterDoesNotCancelCustomTap(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "echoear-quiz"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	running := make(chan context.Context, 1)
	b.ws.mu.Lock()
	d := b.ws.devices[id]
	b.ws.startTurn(d, time.Now(), false, func(ctx context.Context, _ uint64) { running <- ctx })
	turn := d.turn
	b.ws.mu.Unlock()
	active := <-running
	scene := toolTest(t, base, "scene", object{"device_id": id, "nodes": []object{
		{"id": "answer", "t": "btn", "x": 80, "y": 100, "w": 200, "h": 40, "s": "Answer", "ask": "Do not submit this"}}}, false)
	done := make(chan object, 1)
	go func() {
		out, err := b.ws.waitEvent(t.Context(), id, nil, time.Second)
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	eventually(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return b.ws.hasWaiter(d)
	})
	c.json(t, object{"ev": "tap", "id": "answer", "x": 100, "y": 120})
	ev, _ := (<-done)["event"].(deviceEvent)
	if ev.Type != "tap" || ev.Data["id"] != "answer" {
		t.Fatal(ev)
	}
	b.ws.mu.Lock()
	stillRunning := d.turn == turn && d.busy && d.agentSetScene && active.Err() == nil
	b.ws.mu.Unlock()
	if !stillRunning {
		t.Fatal("custom tap replaced the active turn")
	}
	after := toolTest(t, base, "wait_event", object{"device_id": id, "after_cursor": scene["event_cursor"], "timeout_ms": 0}, false)
	if after["event"].(object)["type"] != "tap" {
		t.Fatal(after)
	}
	if result := toolTest(t, base, "wait_event", object{"device_id": id, "timeout_ms": 10}, false); result["event"] != nil {
		t.Fatal(result)
	}
	view := noticeScene("Should not show", "Legacy")
	b.ws.finishTurn(d, turn, &view)
	if snap := toolTest(t, base, "snapshot", object{"device_id": id}, false); snap["scene"].(object)["custom_scene"] != true {
		t.Fatal("finish overwrote the custom scene")
	}
	for _, disconnect := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		errCh := make(chan error, 1)
		go func() {
			_, err := b.ws.waitEvent(ctx, id, nil, time.Second)
			errCh <- err
		}()
		eventually(t, func() bool {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			return b.ws.hasWaiter(d)
		})
		if disconnect {
			c.conn.Close()
		} else {
			cancel()
		}
		if err := <-errCh; err == nil {
			t.Fatal("wait not interrupted")
		}
		cancel()
	}
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

func expectChatAndPCM(t *testing.T, client *testWS, text string) {
	t.Helper()
	match := chatFrame(text)
	chat, audio := false, false
	client.until(t, func(f wsFrame) bool {
		chat = chat || match(f)
		audio = audio || f.op == wsBinary && bytes.Equal(f.data, []byte{pcmPrefix, 1, 2, 3, 4})
		return chat && audio
	})
}

func TestMCPSpeakChimeAndIntercom(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.worker = fakeTTS(b.root, nil)
		b.history = loadChatHistory(filepath.Join(dir, "chat_events.json"))
		b.virtual = loadVirtual(filepath.Join(dir, "virtual_clients.json"))
	})
	const from = "echoear-source"
	to := str(testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Plum", "theme": "plum"}, 201))["id"])
	sender := testConnectWS(t, base, "unit", false)
	sender.hello(t, from, "Cake")
	recipient := testConnectWS(t, base, "unit", false)
	recipient.hello(t, to, "Ignored")
	watcher := testConnectWS(t, base, "unit", false)
	watcher.json(t, object{"ev": "hello", "role": "web"})
	watcher.until(t, hasWS("peers", to))
	watcher.json(t, object{"ev": "watch", "device_id": to})
	watcher.until(t, hasWS("scene", ""))
	b.ws.mu.Lock()
	d := b.ws.devices[to]
	b.ws.startTurn(d, time.Now(), false, func(context.Context, uint64) {})
	b.ws.mu.Unlock()
	toolTest(t, base, "chime", object{"device_id": to, "kind": "tap"}, false)
	recipient.until(t, hasWS("chime", `"kind":"tap"`))
	toolTest(t, base, "speak", object{"device_id": to, "text": "Good listening!"}, false)
	expectChatAndPCM(t, recipient, "Good listening!")
	watcher.until(t, chatFrame("Good listening!"))
	b.ws.mu.Lock()
	spoke := d.agentSpoke
	b.ws.mu.Unlock()
	if !spoke {
		t.Fatal("speak did not mark the active turn")
	}
	toolTest(t, base, "intercom", object{"from_device_id": from, "to_device_id": "Plum", "text": "Dinner is ready."}, true)
	toolTest(t, base, "intercom", object{"from_device_id": from, "to_device_id": to, "text": "Dinner is ready."}, false)
	expectChatAndPCM(t, recipient, "Dinner is ready.")
	watcher.until(t, chatFrame("Dinner is ready."))
	wait := toolTest(t, base, "wait_event", object{"device_id": to, "after_cursor": 0, "timeout_ms": 0}, false)
	if wait["event"].(object)["type"] != "intercom" {
		t.Fatal(wait)
	}
	for _, id := range []string{from, to} {
		got := string(testRequest(t, "GET", base+"/api/devices/"+id+"/messages", "unit", nil, 200))
		if !strings.Contains(got, `"source":"intercom"`) || !strings.Contains(got, "Dinner is ready.") {
			t.Fatal(got)
		}
	}
	pcm := make(chan []byte, 2)
	httpDevice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/speak" || r.Header.Get("X-EchoEar-Token") != "unit" {
			t.Error("incorrect PCM fallback request")
		}
		raw, _ := io.ReadAll(r.Body)
		pcm <- raw
		w.WriteHeader(204)
	}))
	defer httpDevice.Close()
	host, port := httpDevice.Listener.Addr().(*net.TCPAddr).IP.String(), httpDevice.Listener.Addr().(*net.TCPAddr).Port
	if err := b.peers.register(peer{"legacy", "Mint", host, port, time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	toolTest(t, base, "speak", object{"device_id": "legacy", "text": "Hello!"}, false)
	toolTest(t, base, "chime", object{"device_id": "legacy", "kind": "celebrate"}, false)
	if !bytes.Equal(<-pcm, []byte{1, 2, 3, 4}) || !bytes.Equal(<-pcm, chimePCM("celebrate")) {
		t.Fatal("invalid HTTP speech/chime PCM")
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
