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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockTunnel struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *mockTunnel) run(_ context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, slices.Clone(args))
	if slices.Equal(args, []string{"status"}) {
		return []byte("Hostname: home.opentunnel.example\nunrelated.opentunnel.example → 127.0.0.1:1234\nService: running\n"), nil
	}
	if len(args) == 5 && args[0] == "route" && args[3] == "--profile" && profileKey.MatchString(args[4]) {
		switch args[1] {
		case "add":
			if strings.HasPrefix(args[2], "127.0.0.1:") {
				return []byte("Route added: https://scoped.home.opentunnel.example → " + args[2]), nil
			}
		case "remove":
			if args[2] == "scoped" {
				return nil, nil
			}
		}
	}
	return nil, fmt.Errorf("unexpected mock CLI arguments")
}

func apiObjects(t *testing.T, url, key string) []any {
	t.Helper()
	return testObject(t, testRequest(t, "GET", url, "unit", nil, 200))[key].([]any)
}

func TestVirtualClientsPersistenceAndDeviceMerge(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = loadVirtual(filepath.Join(dir, "virtual_clients.json"))
	})
	if got := apiObjects(t, base+"/api/virtual-clients", "virtual_clients"); len(got) != 0 {
		t.Fatal(got)
	}
	for _, input := range []object{
		{"character": "##", "theme": "plum"},
		{"character": "Plum", "theme": "chartreuse"},
	} {
		testRequest(t, "POST", base+"/api/virtual-clients", "unit", input, 400)
	}
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "  Violet  ", "theme": "plum"}, 201))
	id := str(v["id"])
	if v["character"] != "Violet" || v["theme"] != "plum" || v["online"] != false || !virtualID.MatchString(id) || v["created_at"] == "" {
		t.Fatal(v)
	}
	reload := loadVirtual(b.virtual.path)
	if reload.data[id].Character != "Violet" {
		t.Fatal("virtual client not persisted")
	}
	physical := testConnectWS(t, base, "unit", false)
	physical.hello(t, "echoear-1234", "Cake")
	devices := apiObjects(t, base+"/api/devices", "devices")
	if len(devices) != 2 || devices[0].(object)["kind"] != "physical" || devices[0].(object)["ws_connected"] != true ||
		devices[1].(object)["kind"] != "virtual" || devices[1].(object)["online"] != false || devices[1].(object)["device_id"] != id {
		t.Fatal(devices)
	}
	virtual := testConnectWS(t, base, "unit", false)
	scene := virtual.hello(t, id, "Ignored")
	if !bytes.Contains(scene.data, []byte("#D8C7FF")) {
		t.Fatal("configured theme ignored")
	}
	if v := apiObjects(t, base+"/api/virtual-clients", "virtual_clients")[0].(object); v["ws_connected"] != true {
		t.Fatal(v)
	}
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 204)
	virtual.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 404)
	if len(loadVirtual(b.virtual.path).data) != 0 || len(apiObjects(t, base+"/api/devices", "devices")) != 1 {
		t.Fatal("deleted virtual client still visible")
	}
}

func TestScopedTunnelIsolationAndOwnedLifecycle(t *testing.T) {
	r := &mockTunnel{}
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			var input ocSettings
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			for _, action := range []string{"execute", "webfetch", "echoear_*"} {
				if !slices.Contains(input.Permissions, ocPermission{action, "*", "deny"}) {
					t.Errorf("public session did not explicitly deny %s", action)
				}
			}
			_, _ = io.WriteString(w, `{"data":{"id":"ses_public"}}`)
		case "/api/session/ses_public/prompt":
			var input struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !strings.Contains(input.Text, "public chat (spoken prose only); other EchoEars: none") {
				t.Error("public prompt left its scope")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg-user"}}`)
		case "/api/experimental/session/ses_public/wait":
			w.WriteHeader(204)
		case "/api/session/ses_public/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg-answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Hello!"}]},{"id":"msg-user","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oc.Close()
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = loadVirtual(filepath.Join(dir, "virtual_clients.json"))
		b.tunnels = newTunnelManager(b, filepath.Join(dir, "tunnels.json"), "mock-opentunnel", r)
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		settings, err := openCodeSettings("echoear", "unit/test")
		if err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			filepath.Join(dir, "sessions.json"), dir, settings)
	})
	defer b.tunnels.shutdown()
	id := str(testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Plum", "theme": "plum"}, 201))["id"])
	status := testObject(t, testRequest(t, "GET", base+"/api/tunnel/status", "unit", nil, 200))
	if len(status) != 4 || status["installed"] != true || status["service_running"] != true || status["hostname"] != "home.opentunnel.example" {
		t.Fatal(status)
	}
	testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": "admin"}, 404)
	testRequest(t, "DELETE", base+"/api/tunnels/virt-00000000", "unit", nil, 404)
	result := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": id}, 201))
	if result["active"] != true || result["route_name"] != "scoped" || result["device_id"] != id {
		t.Fatal("bad tunnel response")
	}
	again := testObject(t, testRequest(t, "POST", base+"/api/tunnels", "unit", object{"device_id": id}, 201))
	if again["share_url"] != result["share_url"] {
		t.Fatal("tunnel add not idempotent")
	}
	if details := apiObjects(t, base+"/api/devices", "devices")[0].(object)["tunnel"].(object); details["active"] != true {
		t.Fatal("unified list missing tunnel")
	}
	stat, err := os.Stat(b.tunnels.store.path)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("insecure tunnel file permissions", err)
	}
	restored := newTunnelManager(b, b.tunnels.store.path, "mock-opentunnel", r)
	defer restored.shutdown()
	b.tunnels.mu.Lock()
	before := b.tunnels.store.data[id]
	live := b.tunnels.live[id]
	delete(b.tunnels.live, id)
	b.tunnels.mu.Unlock()
	live.close()
	restored.restore()
	if info := restored.info(id); info == nil || info["active"] != true || info["share_url"] != result["share_url"] {
		t.Fatal("scoped listener did not restore without CLI")
	}
	port := strconv.Itoa(before.LocalPort)
	scoped := "http://" + net.JoinHostPort("127.0.0.1", port)
	token := before.Token
	if !scopeKey.MatchString(token) || !strings.HasSuffix(str(result["share_url"]), "/app?token="+token) {
		t.Fatal("bad scoped share URL")
	}
	physical := testConnectWS(t, base, "unit", false)
	physical.hello(t, "echoear-private", "Cake")
	testRequest(t, "GET", scoped+"/app", "", nil, 200)
	testRequest(t, "GET", scoped+"/app/client.js", "", nil, 200)
	testRequest(t, "GET", scoped+"/api/scope", "unit", nil, 401)
	scope := testObject(t, testRequest(t, "GET", scoped+"/api/scope?token="+token, "", nil, 200))
	if scope["scoped"] != true || scope["device_id"] != id || scope["character"] != "Plum" || scope["theme"] != "plum" {
		t.Fatal(scope)
	}
	for _, path := range []string{"/api/devices", "/api/tunnels", "/api/virtual-clients", "/api/tunnel/status", "/ota/version.json", "/mcp", "/health", "/api/audio/abcd"} {
		testRequest(t, "GET", scoped+path, token, nil, 404)
	}
	testRequest(t, "POST", scoped+"/mcp", token, object{}, 404)
	testRequest(t, "GET", scoped+"/api/devices/"+id+"/messages", token, nil, 200)
	testRequest(t, "GET", scoped+"/api/devices/echoear-private/messages", token, nil, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": "echoear-private", "text": "Question?"}, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": ""}, 400)
	chat := testObject(t, testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": "Question?"}, 200))
	if chat["reply"] != "Hello!" || chat["device_id"] != id || chat["audio_url"] != nil {
		t.Fatal("public chat did not return a scoped reply")
	}
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": "echoear-private"}, 403)
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": id}, 200)
	_, rejected := dialTestWS(t, scoped, "unit", false, nil)
	if rejected.StatusCode != 401 {
		t.Fatal(rejected.StatusCode)
	}
	rejected.Body.Close()
	_, rejected = dialTestWS(t, scoped, token, false, object{"Origin": "https://other.example"})
	if rejected.StatusCode != 403 {
		t.Fatal(rejected.StatusCode)
	}
	rejected.Body.Close()
	conn, upgrade := dialTestWS(t, scoped, token, true, object{"Origin": "https://127.0.0.1:" + port})
	if upgrade.StatusCode != 101 {
		t.Fatal(upgrade.StatusCode)
	}
	conn.json(t, object{"ev": "hello", "role": "device", "device_id": "echoear-private", "character": "Not allowed"})
	conn.until(t, hasWS("error", `"status":403`))
	conn.hello(t, id, "Ignored")
	peers := conn.until(t, hasWS("peers", id))
	if bytes.Contains(peers.data, []byte("echoear-private")) {
		t.Fatal("public WebSocket leaked private peer")
	}
	conn.json(t, object{"ev": "watch", "device_id": "echoear-private"})
	conn.until(t, hasWS("error", `"status":403`))
	conn.json(t, object{"ev": "watch", "device_id": id})
	conn.until(t, hasWS("scene", ""))
	conn.json(t, object{"ev": "rec_start", "target": "echoear-private"})
	conn.until(t, hasWS("error", `"status":403`))
	restored.shutdown()
	conn.until(t, func(f wsFrame) bool { return f.op == wsClose })
	testRequest(t, "DELETE", base+"/api/virtual-clients/"+id, "unit", nil, 204)
	if len(apiObjects(t, base+"/api/tunnels", "tunnels")) != 0 {
		t.Fatal("owned route not removed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) != 3 || !slices.Equal(r.calls[0], []string{"status"}) ||
		!slices.Equal(r.calls[1], []string{"route", "add", "127.0.0.1:" + port, "--profile", before.Profile}) ||
		!slices.Equal(r.calls[2], []string{"route", "remove", "scoped", "--profile", before.Profile}) {
		t.Fatal("unexpected OpenTunnel CLI calls")
	}
}

func TestChatHistoryPaginationAndMerge(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/session/ses_test/message" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("limit") != "50" {
			t.Error("unexpected page size", q)
		}
		var data []object
		var next any
		switch q.Get("cursor") {
		case "":
			if q.Get("order") != "asc" {
				t.Error("first page not ascending", q)
			}
			data = []object{{"id": "msg1", "type": "user", "time": object{"created": 1000},
				"metadata": object{"echoEarSource": "voice"},
				"text":     "[EchoEar: you are Cake (device_id=echoear-history); other EchoEars: none]\nWhy is the sky blue?"}}
			next = "opaque+next="
		case "opaque+next=":
			if q.Has("order") {
				t.Error("cursor must omit order", q)
			}
			data = []object{{"id": "msg2", "type": "assistant", "time": object{"created": 3000}, "content": []object{
				{"type": "reasoning", "text": "Private thoughts"},
				{"type": "text", "text": "Air scatters blue light.\n```echoear-screen\n{}\n```"},
				{"type": "tool", "name": "echoear_scene", "state": object{"input": object{"title": "Blue sky"}}},
			}}}
		default:
			t.Error("unexpected opaque cursor", q)
		}
		_ = json.NewEncoder(w).Encode(object{"data": data, "cursor": object{"next": next}})
	}))
	defer upstreamServer.Close()
	_, base := wsHarness(t, func(b *bridge, dir string) {
		b.history = loadChatHistory(filepath.Join(dir, "chat_events.json"))
		file := filepath.Join(dir, "service.json")
		if err := writeMap(file, object{"url": upstreamServer.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = &openCode{upstream: upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: file, read: openCodeEndpoint}},
			store: loadStore(filepath.Join(dir, "sessions.json"), func(_, _ string) bool { return true })}
		if err := b.oc.save("echoear-history", "ses_test"); err != nil {
			t.Fatal(err)
		}
		entry := newChat("intercom", "Mint", "Dinner is ready!", "intercom")
		entry.Timestamp = time.Unix(2, 0).UTC()
		b.history.add("echoear-history", entry)
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, "echoear-history", "Cake")
	raw := testRequest(t, "GET", base+"/api/devices/echoear-history/messages", "unit", nil, 200)
	data := testObject(t, raw)
	messages := data["messages"].([]any)
	if len(messages) != 3 || messages[0].(object)["text"] != "Why is the sky blue?" || messages[0].(object)["source"] != "voice" ||
		messages[1].(object)["text"] != "Dinner is ready!" || messages[2].(object)["text"] != "Air scatters blue light." ||
		messages[2].(object)["scene_title"] != "Blue sky" || messages[2].(object)["tool_summary"] != "Showed a scene" {
		t.Fatal(string(raw))
	}
}

func TestAgentSpeechSuppressesTerminalProse(t *testing.T) {
	var synthesis atomic.Int32
	var b *bridge
	var base string
	const id = "echoear-spoken"
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			_, _ = io.WriteString(w, `{"data":{"id":"ses_speech"}}`)
		case "/api/session/ses_speech/prompt":
			_, err := b.callMCP(r.Context(), "speak", object{"device_id": id, "text": "Already spoken."})
			if err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg-user"}}`)
		case "/api/experimental/session/ses_speech/wait":
			w.WriteHeader(204)
		case "/api/session/ses_speech/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg-answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Done."}]},{"id":"msg-user","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oc.Close()
	b, base = wsHarness(t, func(b *bridge, dir string) {
		b.worker = fakeTTS(b.root, &synthesis)
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			filepath.Join(dir, "sessions.json"), dir, ocSettings{})
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	w := testConnectWS(t, base, "unit", false)
	w.json(t, object{"ev": "hello", "role": "web"})
	w.until(t, hasWS("peers", id))
	w.json(t, object{"ev": "watch", "device_id": id})
	w.until(t, hasWS("scene", ""))
	eventually(t, func() bool { return synthesis.Load() >= 3 }) // Character warm-up.
	before := synthesis.Load()
	c.json(t, object{"ev": "text", "text": "Say something."})
	w.until(t, chatFrame("Already spoken."))
	w.until(t, chatFrame("Say something."))
	w.until(t, chatFrame("Done."))
	eventually(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return !b.ws.devices[id].busy
	})
	if synthesis.Load() != before+1 {
		t.Fatal("terminal prose was synthesized again")
	}
}
