package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "turso.tech/database/tursogo"
)

func testDB(t *testing.T, path, legacy string) *bridgeDB {
	t.Helper()
	d, err := openBridgeDB(t.Context(), path, legacy, "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func writeMap(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func TestTursoDriver(t *testing.T) {
	db, err := sql.Open("turso", filepath.Join(t.TempDir(), "echoear.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE probe (id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO probe VALUES (1, 'ok')"); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.QueryRow("SELECT value FROM probe").Scan(&value); err != nil || value != "ok" {
		t.Fatalf("value = %q, err = %v", value, err)
	}
}

func TestTursoMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echoear.db")
	at := time.Date(2026, 10, 1, 12, 5, 0, 123000000, time.UTC)
	cake := peer{"echoear-0001", "Cake", "192.168.8.21", 80, at.Unix()}
	mint := peer{"echoear-0002", "Mint", "192.168.8.22", 80, at.Unix()}
	greg := virtualClient{ID: "virt-12345678", Character: "Greg", Theme: "plum", CreatedAt: at}
	tunnel := tunnelRecord{greg.ID, "greg", "https://greg.example.test", 1234, strings.Repeat("a", 32), "echoear-" + strings.Repeat("b", 24), ""}
	chat := chatEntry{ID: "legacy-1", Role: "system", Sender: "Greg", Text: "First", Timestamp: at, Source: "mcp", SceneTitle: "Quiz", ToolSummary: "Card"}
	sessions := map[string]string{cake.DeviceID: "ses_cake", mint.DeviceID: "ses_mint", greg.ID: "ses_greg"}
	voices := map[string]string{cake.DeviceID: heartVoice, mint.DeviceID: fableVoice}
	all := map[string]any{
		"devices":         object{cake.DeviceID: cake, mint.DeviceID: mint, "bad": []int{1}},
		"virtual_clients": map[string]virtualClient{greg.ID: greg},
		"tunnels":         map[string]tunnelRecord{greg.ID: tunnel},
		"sessions":        sessions,
		"voices":          voices,
		"chat_events":     object{greg.ID: []any{chat, "bad", chatEntry{ID: "legacy-2", Timestamp: at}}},
	}
	backups := make(map[string][]byte)
	for name, rows := range all {
		backup := filepath.Join(dir, name+".json")
		if err := writeMap(backup, rows); err != nil {
			t.Fatal(err)
		}
		backups[backup], _ = os.ReadFile(backup)
	}
	db := testDB(t, path, dir)
	assertRows := func(name string, n int) {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + name).Scan(&count); err != nil || count != n {
			t.Fatalf("%s rows = %d, want %d: %v", name, count, n, err)
		}
	}
	for name, n := range map[string]int{"meta": 1, "devices": 2, "virtual_clients": 1, "tunnels": 1, "sessions": 3,
		"voices": 2, "chat_events": 2, "memory": 0, "assets": 0, "timers": 0} {
		assertRows(name, n)
	}
	if got, _, err := deviceStore(db).get(cake.DeviceID); err != nil || got != cake {
		t.Fatal("device fields did not migrate", err)
	}
	if got, _, err := virtualStore(db).get(greg.ID); err != nil || got != greg {
		t.Fatal("virtual client fields did not migrate", err)
	}
	if got, _, err := tunnelStore(db).get(greg.ID); err != nil || got.CreatedAt == "" || got.Token != tunnel.Token || got.URL != tunnel.URL {
		t.Fatal("tunnel fields did not migrate", err)
	}
	if got, err := sessionStore(db).all(); err != nil || !reflect.DeepEqual(got, sessions) {
		t.Fatal("sessions did not migrate", err)
	}
	if got, err := voiceStore(db).all(); err != nil || !reflect.DeepEqual(got, voices) {
		t.Fatal("voices did not migrate", err)
	}
	if got, err := (&chatHistory{db}).list(t.Context(), greg.ID); err != nil || len(got) != 2 || got[0] != chat {
		t.Fatal("chat events did not migrate", err)
	}
	for backup, original := range backups {
		after, err := os.ReadFile(backup)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("backup %s changed: %v", filepath.Base(backup), err)
		}
	}
	if err := sessionStore(db).save(cake.DeviceID, "", true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDB(t, path, dir)
	assertRows("sessions", 2)
	assertRows("chat_events", 2)
	var marker string
	if err := db.QueryRow("SELECT value FROM meta WHERE key='migrated_json_v1'").Scan(&marker); err != nil || marker != "1" {
		t.Fatal("migration marker missing", err)
	}
}

func TestCloudOptionWorksOffline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echoear.db")
	local := testDB(t, path, "")
	if err := voiceStore(local).save("offline", heartVoice, false); err != nil {
		t.Fatal(err)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unavailable.Close()
	db, err := openBridgeDB(t.Context(), path, "", unavailable.URL, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if db.cloud == nil {
		t.Fatal("cloud sync not configured")
	}
	if err := db.syncCloud(t.Context()); err == nil {
		t.Fatal("false sync success against unavailable server")
	}
	if got, _, err := voiceStore(db).get("offline"); err != nil || got != heartVoice {
		t.Fatal("cloud configuration lost local data", err)
	}
	if err := voiceStore(db).save("offline", fableVoice, false); err != nil {
		t.Fatal("cloud error blocked local write", err)
	}
}
