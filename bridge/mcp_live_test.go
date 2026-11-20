//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestLiveEchoEarCodeMode(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	config := liveConfig(t, home)
	port := config["ECHOEAR_BRIDGE_PORT"]
	if port == "" {
		port = "8765"
	}
	client := pooledClient()
	client.Timeout = 105 * time.Second
	defer client.CloseIdleConnections()
	c := liveClient{client, env("ECHOEAR_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port)), config["ECHOEAR_BRIDGE_TOKEN"]}
	catalog := liveJSON[struct {
		Result struct{ Tools []struct{ Name string } }
	}](t, c, "/mcp", object{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, nil, "", nil)
	names := []string{}
	for _, tool := range catalog.Result.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Equal(names, []string{"devices", "snapshot", "scene", "patch", "speak", "chime", "intercom", "control", "memory", "asset", "timer", "wait_event"}) {
		t.Fatal("unexpected live MCP catalog", names)
	}
	t.Logf("MCP catalog: %v", names)
	const character = "LiveMCP"
	const clients = "/api/virtual-clients"
	v := testObject(t, testRequest(t, http.MethodPost, c.base+clients, c.token,
		object{"character": character, "theme": "cake"}, http.StatusCreated))
	id := str(v["id"])
	if !virtualID.MatchString(id) {
		t.Fatal("unexpected virtual client ID")
	}
	defer testRequest(t, http.MethodDelete, c.base+clients+"/"+id, c.token, nil, http.StatusNoContent)
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	api := upstream{"OpenCode", client, &fileCache[endpoint]{path: filepath.Join(state, "opencode/service.json"), read: openCodeEndpoint}}
	mcp, err := ocData[[]struct {
		Name   string
		Status struct{ Status string }
	}](t.Context(), api, http.MethodGet, "/api/mcp", nil)
	if err != nil || !slices.ContainsFunc(mcp, func(server struct {
		Name   string
		Status struct{ Status string }
	}) bool {
		return server.Name == "echoear" && server.Status.Status == "connected"
	}) {
		t.Fatalf("OpenCode echoear MCP server is not connected: %v", err)
	}
	model := config["ECHOEAR_OPENCODE_MODEL"]
	if model == "" {
		model = defaultOpenCodeModel
	}
	settings, err := openCodeSettings("echoear", model)
	if err != nil {
		t.Fatal(err)
	}
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	prefix := "echoear-live-mcp-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	liveSessions(t, api, prefix)
	project, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	o := newOpenCode(root, api, testDB(t, ":memory:", ""), project, settings)
	prompt := turnPrompt(peer{DeviceID: id, Character: character}, false, "typed",
		"Run a read-only screen check. Use Code Mode execute to call tools.echoear.devices({}), tools.echoear.snapshot({device_id: '"+id+
			"'}), tools.echoear.memory({op: 'list', scope: '"+id+"'}), tools.echoear.asset({op: 'list'}), and tools.echoear.timer({op: 'list', device_id: '"+
			id+"'}). Return all five tool results from execute, then say one short sentence. Do not call any other tool.")
	_, sid, err := o.ask(root, peer{DeviceID: prefix, Character: character}, prompt, nil, "typed", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertLiveActor(t, api, sid, settings)
	type toolPart struct {
		Type, Name string
		State      struct {
			Status         string
			Input, Content json.RawMessage
		}
	}
	msgs, err := ocData[[]struct{ Content []toolPart }](t.Context(), api, http.MethodGet, "/api/session/"+sid+"/message?order=asc&limit=50", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"devices": "ws_connected", "snapshot": "custom_scene", "memory": "entries", "asset": "assets", "timer": "timers"}
	attempted := map[string]bool{}
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type != "tool" || part.Name != "execute" || part.State.Status != "completed" {
				continue
			}
			for tool, field := range want {
				called := bytes.Contains(part.State.Input, []byte("tools.echoear."+tool))
				attempted[tool] = attempted[tool] || called
				if called && bytes.Contains(part.State.Content, []byte(field)) {
					delete(want, tool)
				}
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("Code Mode results not seen: %v (tool references: %v)", want, attempted)
	}
	t.Log("echoear agent completed devices, snapshot, memory, asset, and timer through execute")
}
