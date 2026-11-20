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
		b.virtual = virtualStore(b.db)
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
	if v, _, err := virtualStore(b.db).get(id); err != nil || v.Character != "Violet" {
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
	if clients, err := virtualStore(b.db).all(); err != nil || len(clients) != 0 || len(apiObjects(t, base+"/api/devices", "devices")) != 1 {
		t.Fatal("deleted virtual client still visible")
	}
}

func TestScopedTunnelIsolationAndOwnedLifecycle(t *testing.T) {
	r := &mockTunnel{}
	settings, err := openCodeSettings("echoear", "unit/test")
	if err != nil {
		t.Fatal(err)
	}
	var creates atomic.Int32
	prompts := make(chan string, 3)
	oc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			if r.Method == http.MethodGet {
				_, _ = io.WriteString(w, `{"data":[]}`)
				return
			}
			creates.Add(1)
			var input ocSettings
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !slices.Equal(input.Permissions, settings.Permissions) {
				t.Error("virtual cat did not get standard tool permissions")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"ses_shared"}}`)
		case "/api/session/ses_shared":
			_ = json.NewEncoder(w).Encode(object{"data": struct {
				ID string `json:"id"`
				ocSettings
			}{"ses_shared", settings}})
		case "/api/session/ses_shared/prompt":
			var input struct{ Text string }
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			select {
			case prompts <- input.Text:
			default:
				t.Error("unexpected OpenCode turn")
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg-user"}}`)
		case "/api/experimental/session/ses_shared/wait":
			w.WriteHeader(204)
		case "/api/session/ses_shared/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg-answer","type":"assistant","finish":"stop","content":[{"type":"text","text":"Hello!"}]},{"id":"msg-user","type":"user"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer oc.Close()
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.tunnels = newTunnelManager(b, "mock-opentunnel", r)
		b.history = &chatHistory{b.db}
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"transcript":"Voice to Cake."}`)
		})
		path := filepath.Join(dir, "service.json")
		if err := writeMap(path, object{"url": oc.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
			b.db, dir, settings)
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
	restored := newTunnelManager(b, "mock-opentunnel", r)
	defer restored.shutdown()
	b.tunnels.mu.Lock()
	before, _, err := b.tunnels.store.get(id)
	if err != nil {
		t.Fatal(err)
	}
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
	if scope["scoped"] != true || scope["device_id"] != id || scope["character"] != "Plum" || scope["theme"] != "plum" ||
		scope["cat"].(object)["face_bg"] != "#D8C7FF" {
		t.Fatal(scope)
	}
	for _, path := range []string{"/api/devices", "/api/tunnels", "/api/virtual-clients", "/api/tunnel/status", "/ota/version.json", "/mcp", "/health", "/api/audio/abcd", "/api/cats"} {
		testRequest(t, "GET", scoped+path, token, nil, 404)
	}
	testRequest(t, "POST", scoped+"/mcp", token, object{}, 404)
	testRequest(t, "POST", scoped+"/api/devices/"+id+"/cat", token, object{"name": "No"}, 404)
	testRequest(t, "DELETE", scoped+"/api/cats/plum", token, nil, 404)
	testRequest(t, "GET", scoped+"/api/devices/"+id+"/messages", token, nil, 200)
	testRequest(t, "GET", scoped+"/api/devices/echoear-private/messages", token, nil, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": "echoear-private", "text": "Question?"}, 403)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": ""}, 400)
	chat := testObject(t, testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": "Question?"}, 200))
	if chat["reply"] != "Hello!" || chat["device_id"] != id || chat["session_id"] != "ses_shared" || chat["audio_url"] != nil {
		t.Fatal("tunneled chat did not return a scoped reply")
	}
	header := "[EchoEar: Plum (" + id + "), web, typed, time="
	assertPrompt := func(text string) {
		t.Helper()
		select {
		case got := <-prompts:
			if !strings.HasPrefix(got, header) || !strings.Contains(got, ", screen=face]") || stripEchoEarHeader(got) != text {
				t.Fatal("virtual cat prompt identity or provenance is incorrect")
			}
		default:
			t.Fatal("did not receive an OpenCode prompt")
		}
	}
	assertPrompt("Question?")
	lan := testObject(t, testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": id, "text": "Another question?"}, 200))
	assertPrompt("Another question?")
	if lan["session_id"] != chat["session_id"] || creates.Load() != 1 {
		t.Fatal("LAN and tunnel did not reuse one OpenCode session")
	}
	history := "/api/devices/" + id + "/messages"
	tunneled := testRequest(t, "GET", scoped+history, token, nil, 200)
	if !bytes.Contains(tunneled, []byte("Hello!")) || !bytes.Equal(tunneled, testRequest(t, "GET", base+history, "unit", nil, 200)) {
		t.Fatal("LAN and tunnel histories differ")
	}
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": "echoear-private"}, 404)
	testRequest(t, "POST", scoped+"/api/reset", token, object{"device_id": id}, 404)
	testRequest(t, "POST", scoped+"/api/cats/reassign", token, object{"from_device_id": id, "to_device_id": "echoear-private"}, 404)
	testRequest(t, "POST", scoped+"/api/chat", token, object{"device_id": id, "text": "Tell Cake Hi from HTTP."}, 200)
	physical.until(t, hasWS("patch", "Hi from HTTP."))
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
	if !bytes.Contains(peers.data, []byte(`"character":"Cake","device_id":"echoear-private","online":true`)) ||
		bytes.Contains(peers.data, []byte(`"ip"`)) {
		t.Fatal("scoped WebSocket did not expose the minimal online peer list")
	}
	conn.json(t, object{"ev": "watch", "device_id": "echoear-private"})
	conn.until(t, hasWS("error", `"status":403`))
	conn.json(t, object{"ev": "watch", "device_id": id})
	conn.until(t, hasWS("scene", ""))
	conn.json(t, object{"ev": "text", "text": "WebSocket question?"})
	conn.until(t, chatFrame("Hello!"))
	assertPrompt("WebSocket question?")
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "text", "text": "Tell Cake Hi from text."})
	physical.until(t, hasWS("patch", "Hi from text."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "intercom", "target": "echoear-private", "text": "Hi from WS intercom."})
	physical.until(t, hasWS("patch", "Hi from WS intercom."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "tap", "id": "cat", "x": 180, "y": 316})
	conn.until(t, hasWS("scene", `"cat-ring"`))
	conn.json(t, object{"ev": "tap", "id": "text", "x": 130, "y": 300})
	conn.until(t, func(f wsFrame) bool { return hasWS("scene", "Msg Cake")(f) || hasWS("patch", "Msg Cake")(f) })
	conn.json(t, object{"ev": "tap", "id": "msg-0", "x": 180, "y": 150})
	conn.until(t, hasWS("rec", `"target":"echoear-private"`))
	conn.json(t, object{"ev": "rec_start", "target": "echoear-private"})
	pcm := bytes.Repeat([]byte{12, 0}, 120)
	conn.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, pcm...)))
	conn.json(t, object{"ev": "rec_end", "target": "echoear-private"})
	got := physical.until(t, func(f wsFrame) bool { return f.op == wsBinary })
	if got.data[0] != pcmPrefix || !bytes.Equal(got.data[1:], pcm) {
		t.Fatal("tunneled drawer did not deliver recorded PCM")
	}
	physical.until(t, hasWS("patch", "Voice to Cake."))
	conn.until(t, hasWS("mode", `"mode":"idle"`))
	conn.json(t, object{"ev": "tap", "id": "cat", "x": 180, "y": 316})
	conn.until(t, hasWS("scene", `"cat-ring"`))
	conn.json(t, object{"ev": "reset", "device_id": id})
	conn.json(t, object{"ev": "tap", "id": "new", "x": 230, "y": 104})
	conn.json(t, object{"ev": "watch", "device_id": id})
	conn.until(t, hasWS("scene", `"bg":"#D8C7FF"`))
	testRequest(t, "POST", base+"/api/reset", "unit", object{"device_id": id}, 200)
	conn.until(t, hasWS("scene", `"bg":"#F3F4F6"`))
	conn.until(t, hasWS("cat", `"configured":false`))
	scope = testObject(t, testRequest(t, "GET", scoped+"/api/scope", token, nil, 200))
	if scope["character"] != "New Cat" || scope["cat"].(object)["configured"] != false {
		t.Fatal("scope retained the old cat after reset", scope)
	}
	if bytes.Contains(testRequest(t, "GET", base+history, "unit", nil, 200), []byte("Hello!")) {
		t.Fatal("LAN reset did not reset the shared conversation")
	}
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

func TestVirtualCatSessionAliasMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	initial := map[string]string{
		"scope-virt-12345678": "ses_tunnel",
		"scope-virt-abcdef12": "ses_old",
		"virt-abcdef12":       "ses_lan",
		"echoear-physical":    "ses_cake",
	}
	if err := writeMap(path, initial); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	db := testDB(t, ":memory:", dir)
	o := newOpenCode(ctx, upstream{}, db, "", ocSettings{})
	for id, want := range map[string]string{"virt-12345678": "ses_tunnel", "virt-abcdef12": "ses_lan", "echoear-physical": "ses_cake"} {
		got, err := o.get(id)
		if err != nil || got != want {
			t.Fatalf("%s did not keep its existing session", id)
		}
	}
	if got, err := sessionStore(db).all(); err != nil || len(got) != 5 {
		t.Fatal("legacy aliases were not preserved alongside canonical sessions")
	}
}

func TestChatHistoryPaginationAndMerge(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sendPage := func(data []object, next any) {
			_ = json.NewEncoder(w).Encode(object{"data": data, "cursor": object{"next": next}})
		}
		switch r.URL.Path {
		case "/api/session":
			if q.Get("limit") != "200" {
				t.Error("unexpected session page size", q)
			}
			switch q.Get("cursor") {
			case "":
				sendPage([]object{
					{"id": "ses_latest", "title": "EchoEar: Cake (ECHOEAR-HISTORY)", "time": object{"created": 300}},
					{"id": "ses_other", "title": "EchoEar: echoear-history2", "time": object{"created": 100}},
					{"id": "ses_active", "title": "Manually renamed", "time": object{"created": 300}},
				}, "session+next=")
			case "session+next=":
				sendPage([]object{
					{"id": "ses_empty", "title": "EchoEar: scope-echoear-history", "time": object{"created": 200}},
					{"id": "ses_oldest", "title": "EchoEar: echoear-history", "time": object{"created": 100}},
				}, nil)
			default:
				t.Error("unexpected session cursor", q)
			}
			return
		case "/api/session/ses_empty/message":
			sendPage([]object{{"id": "msg_idle", "type": "idle"}}, nil)
			return
		case "/api/session/ses_active/message":
			sendPage([]object{
				{"id": "msg_active", "type": "user", "text": "[EchoEar: Cake (echoear-history), hardware, tap]\nContinuing.",
					"metadata": object{"echoEarSource": "tap"}, "time": object{"created": 4000}},
				{"id": "msg2", "type": "user", "text": "Duplicate.", "time": object{"created": 4100}},
			}, nil)
			return
		case "/api/session/ses_latest/message":
			sendPage([]object{{"id": "msg_latest", "type": "user", "text": "[EchoEar: you are Cake (device_id=echoear-history)]\nLatest.",
				"time": object{"created": 5000}}}, nil)
			return
		case "/api/session/ses_other/message":
			t.Error("requested messages for a different device")
			http.NotFound(w, r)
			return
		case "/api/session/ses_oldest/message":
		default:
			http.NotFound(w, r)
			return
		}
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
				"text":     "[EchoEar: Cake (echoear-history), hardware, voice]\nWhy is the sky blue?"}}
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
		sendPage(data, next)
	}))
	defer upstreamServer.Close()
	_, base := wsHarness(t, func(b *bridge, dir string) {
		b.history = &chatHistory{b.db}
		file := filepath.Join(dir, "service.json")
		if err := writeMap(file, object{"url": upstreamServer.URL, "password": "unit"}); err != nil {
			t.Fatal(err)
		}
		b.oc = newOpenCode(b.root, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: file, read: openCodeEndpoint}},
			b.db, dir, ocSettings{})
		if err := b.oc.save("echoear-history", "ses_active"); err != nil {
			t.Fatal(err)
		}
		entry := newChat("intercom", "Mint", "Dinner is ready!", "intercom")
		entry.ID = "bridge-dinner"
		entry.Timestamp = time.Unix(2, 0).UTC()
		b.history.add("echoear-history", entry)
		b.history.add("echoear-history", entry)
		b.history.add("echoear-history", chatEntry{ID: "msg2", Role: "assistant", Text: "Local duplicate."})
	})
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, "echoear-history", "Cake")
	raw := testRequest(t, "GET", base+"/api/devices/echoear-history/messages", "unit", nil, 200)
	var data struct{ Messages []chatEntry }
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, message := range data.Messages {
		ids = append(ids, message.ID)
	}
	if want := []string{"msg1", "bridge-dinner", "msg2", "divider-ses_active", "msg_active", "divider-ses_latest", "msg_latest"}; !slices.Equal(ids, want) {
		t.Fatalf("message order = %v, want %v", ids, want)
	}
	messages := data.Messages
	if messages[0].Text != "Why is the sky blue?" || messages[0].Source != "voice" ||
		messages[1].Role != "intercom" || messages[1].Text != "Dinner is ready!" ||
		messages[2].Sender != "Cake" || messages[2].Text != "Air scatters blue light." ||
		messages[2].SceneTitle != "Blue sky" || messages[2].ToolSummary != "Showed a scene" ||
		messages[4].Text != "Continuing." || messages[4].Source != "tap" ||
		messages[6].Text != "Latest." || messages[6].Source != "text" {
		t.Fatal(string(raw))
	}
	for _, i := range []int{3, 5} {
		divider := messages[i]
		if divider.Role != "system" || divider.Sender != "System" || divider.Source != "system" ||
			divider.Text != "Started a new chat" || !divider.Timestamp.Equal(messages[i+1].Timestamp) {
			t.Errorf("invalid new chat divider: %+v", divider)
		}
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
			b.db, dir, ocSettings{})
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
