//go:build live

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
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
	if !slices.Equal(names, []string{"inspect", "run_app", "act", "store"}) {
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
	sock := testConnectWS(t, c.base, c.token, true)
	sock.json(t, object{"ev": "hello", "role": "web", "device_id": id})
	sock.until(t, hasWS("svg", "cat-ring"))
	api := liveOpenCode(t, client, home, config)
	type serverInfo struct {
		Name   string
		Status struct{ Status string }
	}
	// A location's first API request can return before MCP autoconnect finishes.
	ready, cancelReady := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelReady()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	var mcp []serverInfo
	for {
		mcp, err = ocData[[]serverInfo](ready, api, http.MethodGet, "/api/mcp", nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.ContainsFunc(mcp, func(server serverInfo) bool {
			return server.Name == "echoear" && server.Status.Status == "connected"
		}) {
			break
		}
		select {
		case <-ready.Done():
			t.Fatalf("OpenCode echoear MCP server did not connect: %+v", mcp)
		case <-tick.C:
		}
	}
	if config["ECHOEAR_OPENCODE_URL"] != "" {
		type plugin struct{ Source struct{ Type string } }
		plugins, err := ocData[[]plugin](t.Context(), api, http.MethodGet, "/api/plugin", nil)
		if err != nil || len(mcp) != 1 || slices.ContainsFunc(plugins, func(p plugin) bool { return p.Source.Type != "builtin" }) {
			t.Fatalf("expected only EchoEar MCP and bundled plugins: %v", err)
		}
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
	o := newOpenCode(root, api, testDB(t, ":memory:", ""), liveOpenCodeDir(home, config), settings)
	prompt := turnPrompt(peer{DeviceID: id, Character: character}, false, "typed",
		"Run a live screen and controls check. Use Code Mode execute to call tools.echoear.inspect({device_id:'"+id+
			"'}), tools.echoear.store({op:'list',scope:'"+id+"'}), tools.echoear.act({to:'"+id+
			"',chime:'tap'}), and tools.echoear.run_app({devices:'"+id+
			"',title:'Code Mode Check',svg:'<svg viewBox=\"0 0 360 360\"><circle cx=\"180\" cy=\"180\" r=\"60\" fill=\"gold\"/></svg>'}). Return all four tool results from execute, then say one short sentence. Do not call any other tool.",
		legacyCat(character, "cake"))
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
	want := map[string]string{"inspect": "active_app", "run_app": "hit_ids", "act": "played", "store": "entries"}
	attempted := map[string]bool{}
	var lastResult string
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type != "tool" || part.Name != "execute" || part.State.Status != "completed" {
				continue
			}
			lastResult = truncate(string(part.State.Content), 512)
			for tool, field := range want {
				called := bytes.Contains(part.State.Input, []byte("tools.echoear."+tool)) || bytes.Contains(part.State.Input, []byte(`["`+tool+`"]`))
				attempted[tool] = attempted[tool] || called
				if called && bytes.Contains(part.State.Content, []byte(field)) {
					delete(want, tool)
				}
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("Code Mode results not seen: %v (tool references: %v; latest tool result: %s)", want, attempted, lastResult)
	}
	t.Log("echoear agent completed inspect, run_app, act, and store through execute")
}
