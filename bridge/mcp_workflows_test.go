package main

import (
	"testing"
	"time"
)

func TestMCPTimerUpdatePreservesIdentity(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, _ string) {
		b.timers = newTimerManager(b)
		t.Cleanup(b.timers.close)
	})
	const id = "echoear-timer-edit"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	timer := patchTimerTest(t, base, object{"device_id": id, "in_seconds": 60, "label": "First", "chime": "tap", "say": "Ready", "notes": "C5:150"}, false)
	timerID := str(timer["id"])
	b.timers.mu.Lock()
	old := b.timers.handles[timerID]
	b.timers.mu.Unlock()
	got := patchTimerTest(t, base, object{"device_id": id, "id": timerID, "op": "update", "in_seconds": 120, "label": "Later", "say": "", "notes": []string{}}, false)
	if got["fire_at"] == timer["fire_at"] || got["created_at"] != timer["created_at"] || got["id"] != timerID ||
		got["label"] != "Later" || got["chime"] != "tap" || got["say"] != "" || len(got["notes"].([]any)) != 0 {
		t.Fatal(got)
	}
	if _, claimed := b.timers.claim(timerID, old); claimed {
		t.Fatal("a stale callback claimed the rescheduled timer")
	}
	patchTimerTest(t, base, object{"device_id": "another-cat", "id": timerID, "op": "update", "label": "Wrong owner"}, true)
	patchTimerTest(t, base, object{"device_id": id, "id": timerID, "op": "update", "app": "missing"}, true)
	read := toolTest(t, base, "state", object{"device_id": id, "include": []string{"timers"}}, false)["timers"].([]any)
	if len(read) != 1 || read[0].(object)["label"] != "Later" || read[0].(object)["created_at"] != timer["created_at"] {
		t.Fatal(read)
	}
}

func TestMCPHistoryCursorAndPhotoMetadata(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "echoear-history-page"
	board := testConnectWS(t, base, "unit", false)
	board.helloV2(t, id, "Cake")
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, msg := range []string{"same-a", "same-b", "same-c"} {
		b.history.add(id, chatEntry{ID: msg, Sender: "User", Role: "user", Text: msg, Source: "typed", Timestamp: now})
	}
	if _, err := b.db.Exec(`INSERT INTO photos (id,device_id,mime,data,caption,created_at) VALUES
		('photo-1234abcd',?,'image/png',x'CAFE','A flower',?),
		('photo-2345abcd','other','image/png',x'CAFE','Another flower',?)`, id, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	first := toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages", "photos"}, "messages_limit": 2}, false)
	list := first["messages"].([]any)
	if len(list) != 2 || list[0].(object)["id"] != "same-b" || list[1].(object)["id"] != "same-c" || first["messages_has_more"] != true {
		t.Fatal(first)
	}
	photos := first["photos"].([]any)
	if len(photos) != 1 {
		t.Fatal(photos)
	}
	if p := photos[0].(object); p["id"] != "photo-1234abcd" || p["caption"] != "A flower" || p["mime"] != "image/png" ||
		p["created_at"] == nil || p["device_id"] != id || p["data"] != nil {
		t.Fatal(p)
	}
	next := toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages"},
		"messages_limit": 2, "messages_cursor": first["messages_next_cursor"]}, false)
	list = next["messages"].([]any)
	if len(list) != 1 || list[0].(object)["id"] != "same-a" || next["messages_has_more"] != false || next["messages_next_cursor"] != nil {
		t.Fatal(next)
	}
	toolTest(t, base, "state", object{"device_id": id, "include": []string{"messages"},
		"messages_cursor": object{"before_ts": now.UnixMilli()}}, true)
}
