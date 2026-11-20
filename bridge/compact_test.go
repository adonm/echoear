package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPSchemaHash(t *testing.T) {
	original := mcpSchemaHash()
	if raw, err := hex.DecodeString(original); err != nil || len(raw) != 32 || original != mcpSchemaHash() {
		t.Fatal("schema hash is not a deterministic SHA-256 hex digest")
	}
	for _, key := range []string{"name", "description", "inputSchema"} {
		tools := slices.Clone(mcpTools)
		tools[0] = maps.Clone(tools[0])
		tools[0][key] = "changed"
		if hashMCPSchema(tools, mcpInstructions) == original {
			t.Errorf("changing %s did not change the schema hash", key)
		}
	}
	if hashMCPSchema(mcpTools, mcpInstructions+" More instructions.") == original {
		t.Error("changing instructions did not change the schema hash")
	}
}

func TestSchemaUpgradeCompactsActiveSessions(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var fail atomic.Bool
	fail.Store(true)
	waiting, release := make(chan struct{}, 1), make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/event" {
			w.WriteHeader(404)
			return
		}
		mu.Lock()
		calls = append(calls, r.Method+" "+path)
		mu.Unlock()
		switch {
		case path == "/api/info" && r.Method == http.MethodGet:
			sendJSON(w, 200, object{})
		case path == "/api/experimental/mcp/echoear/connect" && r.Method == http.MethodPost:
			if r.URL.Query().Get("location[directory]") != "/workspace" {
				t.Error("MCP reconnect did not select the bridge workspace")
			}
			w.WriteHeader(204)
		case path == "/api/session/ses-stale":
			w.WriteHeader(404)
		case strings.HasSuffix(path, "/compact") && r.Method == http.MethodPost:
			if raw, _ := io.ReadAll(r.Body); string(raw) != `{"delivery":"queue"}` {
				t.Error("compaction did not queue")
			}
			sid := strings.Split(path, "/")[3]
			sendJSON(w, 200, object{"data": object{"id": "msg-compaction", "sessionID": sid, "type": "compaction"}})
		case strings.HasSuffix(path, "/wait") && r.Method == http.MethodPost:
			if fail.Load() && strings.Contains(path, "ses-cake") {
				waiting <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			w.WriteHeader(204)
		case strings.HasSuffix(path, "/message/msg-compaction") && r.Method == http.MethodGet:
			if strings.Contains(path, "ses-fresh") {
				sendJSON(w, 200, object{"data": object{"id": "msg-compaction", "type": "compaction", "status": "failed",
					"error": object{"type": "compaction.unavailable", "message": "Nothing to compact yet"}}})
				return
			}
			status := "completed"
			if fail.Load() && strings.Contains(path, "ses-cake") {
				status = "failed"
			}
			sendJSON(w, 200, object{"data": object{"id": "msg-compaction", "type": "compaction", "status": status}})
		case path == "/api/session/ses-cake" || path == "/api/session/ses-mint" || path == "/api/session/ses-fresh":
			sendJSON(w, 200, object{"data": object{"id": strings.TrimPrefix(path, "/api/session/")}})
		default:
			t.Errorf("unexpected OpenCode request: %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	open := func(db *bridgeDB) *bridge {
		o := newOpenCode(ctx, upstream{"OpenCode", api.Client(), fixedEndpoint{base: api.URL}}, db, "/workspace", ocSettings{})
		return &bridge{oc: o, db: db}
	}
	b := open(testDB(t, dbPath, ""))
	if err := b.db.setMeta(ctx, mcpSchemaMeta, "old"); err != nil {
		t.Fatal(err)
	}
	for id, sid := range map[string]string{"Cake": "ses-cake", "Mint": "ses-mint", "fresh": "ses-fresh", "stale": "ses-stale", "empty": ""} {
		if err := b.oc.store.save(id, sid, false); err != nil {
			t.Fatal(err)
		}
	}
	checkMeta := func(want string) {
		t.Helper()
		if got, err := b.db.getMeta(ctx, mcpSchemaMeta); err != nil || got != want {
			t.Fatalf("schema marker = %q, %v; want %q", got, err, want)
		}
	}
	first := make(chan error, 1)
	go func() { first <- b.syncSchemaUpgrade(ctx) }()
	expectValue(t, waiting, struct{}{})
	checkMeta("old") // The /compact response admitted the request but hasn't finished it.
	gate, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if unlock, err := b.oc.lock(gate, "Cake"); err == nil {
		unlock()
		t.Fatal("compaction released the turn gate before completion")
	}
	close(release)
	if err := <-first; err == nil {
		t.Fatal("failed compaction was treated as a successful upgrade")
	}
	checkMeta("old")
	mu.Lock()
	calls = nil
	mu.Unlock()
	fail.Store(false)
	if err := b.syncSchemaUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	checkMeta(mcpSchemaHash())
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := open(testDB(t, dbPath, "")).syncSchemaUpgrade(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /api/info",
		"POST /api/experimental/mcp/echoear/connect",
		"GET /api/session/ses-cake", "POST /api/session/ses-cake/compact",
		"POST /api/experimental/session/ses-cake/wait", "GET /api/session/ses-cake/message/msg-compaction",
		"GET /api/session/ses-mint", "POST /api/session/ses-mint/compact",
		"POST /api/experimental/session/ses-mint/wait", "GET /api/session/ses-mint/message/msg-compaction",
		"GET /api/session/ses-fresh", "POST /api/session/ses-fresh/compact",
		"POST /api/experimental/session/ses-fresh/wait", "GET /api/session/ses-fresh/message/msg-compaction",
		"GET /api/session/ses-stale",
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(calls, want) {
		t.Fatalf("OpenCode calls = %v; want %v", calls, want)
	}
}

func TestCompactAPIOnlyOnAuthenticatedLAN(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &bridge{token: "unit"}
	connects := new(atomic.Int32)
	b.oc = testOpenCode(t, ctx, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/ses-cake":
			sendJSON(w, 200, object{"data": object{"id": "ses-cake"}})
		case "/api/session/ses-cake/compact":
			sendJSON(w, 200, object{"data": object{"id": "msg-test", "sessionID": "ses-cake", "type": "compaction"}})
		case "/api/session/ses-cake/message/msg-test":
			sendJSON(w, 200, object{"data": object{"id": "msg-test", "type": "compaction", "status": "completed"}})
		case "/api/experimental/mcp/echoear/connect":
			connects.Add(1)
			w.WriteHeader(204)
		case "/api/experimental/session/ses-cake/wait":
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	})
	if err := b.oc.save("Cake", "ses-cake"); err != nil {
		t.Fatal(err)
	}
	lan := httptest.NewServer(b)
	defer lan.Close()
	scoped := httptest.NewServer(b.scoped(virtualClient{ID: "virt-12345678"}, "scoped"))
	defer scoped.Close()
	const path = "/api/opencode/compact"
	for _, token := range []string{"", "scoped", "unit"} {
		testRequest(t, http.MethodPost, scoped.URL+path, token, nil, 404)
	}
	testRequest(t, http.MethodPost, lan.URL+path+"?token=unit", "", nil, 401)
	testRequest(t, http.MethodPost, lan.URL+path, "scoped", nil, 401)
	testRequest(t, http.MethodGet, lan.URL+path, "unit", nil, 404)
	testRequest(t, http.MethodPost, lan.URL+path, "unit", object{"reconnect_mcp": "yes"}, 400)
	check := func(input any, compacted, skipped []string) {
		t.Helper()
		out := testRequest(t, http.MethodPost, lan.URL+path, "unit", input, 200)
		var got compactResult
		if err := json.Unmarshal(out, &got); err != nil || !got.OK ||
			got.Compacted == nil || !slices.Equal(got.Compacted, compacted) ||
			got.Skipped == nil || !slices.Equal(got.Skipped, skipped) {
			t.Fatalf("unexpected compaction result: %s", out)
		}
	}
	check(nil, []string{"Cake"}, nil)
	check(object{"device_id": "unknown"}, nil, []string{"unknown"})
	check(object{"device_id": "Cake", "reconnect_mcp": true}, []string{"Cake"}, nil)
	if connects.Load() != 1 {
		t.Fatal("manual MCP reconnect should only run when requested")
	}
}
