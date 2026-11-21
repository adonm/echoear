package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestOpenCodeHistoryBackfillResumesAfterInterruptedPage(t *testing.T) {
	const id, sid, cursor = "echoear-history", "ses_fixture", "opaque+next="
	path := filepath.Join(t.TempDir(), "echoear.db")
	attempt, stop := context.WithCancel(t.Context())
	defer stop()
	var firstPages, nextPages atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			sendJSON(w, 200, object{"data": []object{{"id": sid, "title": "EchoEar: Cake (" + id + ")"}}})
		case "/api/session/" + sid + "/message":
			switch r.URL.Query().Get("cursor") {
			case "":
				firstPages.Add(1)
				sendJSON(w, 200, object{"cursor": object{"next": cursor}, "data": []object{
					{"id": "msg_one", "type": "user", "text": "First", "time": object{"created": 1000},
						"metadata": object{"echoEarSource": "voice"}},
				}})
			case cursor:
				if nextPages.Add(1) == 1 {
					stop() // As if the startup budget expired on this HTTP request.
					<-r.Context().Done()
					return
				}
				sendJSON(w, 200, object{"data": []object{
					{"id": "msg_two", "type": "assistant", "time": object{"created": 2000},
						"content": []object{{"type": "text", "text": "Second"}}},
				}})
			default:
				t.Error("wrong resume cursor")
				http.Error(w, "wrong cursor", 400)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	open := func() *bridge {
		db := testDB(t, path, "")
		return &bridge{db: db, peers: peers{deviceStore(db)}, oc: newOpenCode(t.Context(),
			upstream{"OpenCode", api.Client(), fixedEndpoint{base: api.URL}}, db, "", ocSettings{})}
	}
	b := open()
	if err := b.oc.save(id, sid); err != nil {
		t.Fatal(err)
	}
	if err := b.migrateSessionMessages(attempt); err == nil {
		t.Fatal("interrupted page was treated as complete")
	}
	h := &chatHistory{b.db}
	entries, err := h.list(t.Context(), id)
	if err != nil || len(entries) != 1 || entries[0].ID != "msg_one" {
		t.Fatal("first page was not committed", err, entries)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}
	b = open()
	for range 2 {
		if err := b.migrateSessionMessages(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	h.db = b.db
	entries, err = h.list(t.Context(), id)
	if err != nil || len(entries) != 2 || entries[1].ID != "msg_two" || entries[1].Source != "voice" {
		t.Fatal("backfill did not resume with the previous page's source", err, entries)
	}
	if firstPages.Load() != 1 || nextPages.Load() != 2 {
		t.Fatal("resume fetched the first page again or reran after completion")
	}
}
