package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	turso "turso.tech/database/tursogo"
)

var schema = []string{
	`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS devices (device_id TEXT PRIMARY KEY, character TEXT NOT NULL, ip TEXT NOT NULL, port INTEGER NOT NULL, last_seen INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS sessions (device_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS virtual_clients (id TEXT PRIMARY KEY, character TEXT NOT NULL, theme TEXT NOT NULL, voice TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS voices (device_id TEXT PRIMARY KEY, voice TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS cats (device_id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS saved_cats (slug TEXT PRIMARY KEY, name TEXT NOT NULL, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS tunnels (device_id TEXT PRIMARY KEY, route_name TEXT NOT NULL, url TEXT NOT NULL, local_port INTEGER NOT NULL, scoped_token TEXT NOT NULL, profile TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chat_events (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, scene_title TEXT NOT NULL DEFAULT '', tool_summary TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS chat_device_ts ON chat_events (device_id, ts_ms)`,
	`CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL, PRIMARY KEY (scope, key))`,
	`CREATE TABLE IF NOT EXISTS assets (name TEXT PRIMARY KEY, kind TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', data_json TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS timers (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, label TEXT NOT NULL, say TEXT NOT NULL, chime TEXT NOT NULL, wake_agent INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, fire_at TEXT NOT NULL)`,
}

type bridgeDB struct {
	*sql.DB
	cloud *turso.TursoSyncDb
}

func openBridgeDB(ctx context.Context, path, legacy, remote, token, fallbackVoice string) (_ *bridgeDB, err error) {
	if path != ":memory:" {
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if strings.Contains(path, "?") {
			return nil, errors.New("database path cannot contain '?'")
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, err
		}
		chmod := f.Chmod(0o600)
		if err := errors.Join(chmod, f.Close()); err != nil {
			return nil, err
		}
	}
	d := &bridgeDB{}
	if path != ":memory:" && remote != "" && token != "" {
		// Local-first: an unavailable cloud endpoint must not hold up boot.
		bootstrap := false
		d.cloud, err = turso.NewTursoSyncDb(ctx, turso.TursoSyncDbConfig{
			Path: path, RemoteUrl: remote, AuthToken: token, BootstrapIfEmpty: &bootstrap,
		})
		if err != nil {
			return nil, errors.New("cannot initialize optional Turso Cloud sync")
		}
		d.DB, err = d.cloud.Connect(ctx)
	} else {
		d.DB, err = sql.Open("turso", path)
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			d.Close()
		}
	}()
	// Serialize SQL writers and keep one connection alive for :memory:.
	d.SetMaxOpenConns(1)
	for _, query := range schema {
		if _, err := d.ExecContext(ctx, query); err != nil {
			return nil, fmt.Errorf("initialize EchoEar schema: %w", err)
		}
	}
	if err := d.migrate(ctx, legacy); err != nil {
		return nil, fmt.Errorf("migrate legacy EchoEar state: %w", err)
	}
	if err := d.migrateCats(ctx, deviceVoiceStore(d, fallbackVoice)); err != nil {
		return nil, fmt.Errorf("migrate EchoEar cats: %w", err)
	}
	return d, nil
}

func (d *bridgeDB) syncCloud(ctx context.Context) error {
	if d.cloud == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := d.cloud.Push(ctx); err != nil {
		return err
	}
	_, err := d.cloud.Pull(ctx)
	return err
}

func (d *bridgeDB) syncPeriodically(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := d.syncCloud(ctx); err != nil && ctx.Err() == nil {
			log.Print("EchoEar Turso Cloud sync failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type rowScanner interface{ Scan(...any) error }

// Table definitions share the same SQL codecs between migration and runtime.
type store[T any] struct {
	mu                  sync.Mutex
	db                  *sql.DB
	table, key, columns string
	upsert              string
	scan                func(rowScanner) (string, T, error)
	values              func(string, T) []any
}

func (s *store[T]) all() (map[string]T, error) {
	rows, err := s.db.Query("SELECT " + s.columns + " FROM " + s.table)
	if err != nil {
		return nil, problem(502, "Cannot read "+s.table)
	}
	defer rows.Close()
	out := make(map[string]T)
	for rows.Next() {
		id, value, err := s.scan(rows)
		if err != nil {
			return nil, problem(502, "Cannot read "+s.table)
		}
		out[id] = value
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read "+s.table)
	}
	return out, nil
}

func (s *store[T]) get(id string) (T, bool, error) {
	_, value, err := s.scan(s.db.QueryRow("SELECT "+s.columns+" FROM "+s.table+" WHERE "+s.key+"=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return value, false, nil
	}
	if err != nil {
		return value, false, problem(502, "Cannot read "+s.table)
	}
	return value, true, nil
}

func (s *store[T]) save(id string, value T, remove bool) error {
	var err error
	if remove {
		_, err = s.db.Exec("DELETE FROM "+s.table+" WHERE "+s.key+"=?", id)
	} else {
		_, err = s.db.Exec("INSERT INTO "+s.table+" ("+s.columns+") VALUES ("+placeholders(len(s.values(id, value)))+") ON CONFLICT("+s.key+") DO UPDATE SET "+s.upsert, s.values(id, value)...)
	}
	if err != nil {
		return problem(502, "Cannot save "+s.table)
	}
	return nil
}

func placeholders(n int) string { return strings.Repeat("?,", n-1) + "?" }

func deviceStore(d *bridgeDB) *store[peer] {
	return &store[peer]{db: d.DB, table: "devices", key: "device_id", columns: "device_id,character,ip,port,last_seen",
		upsert: "character=excluded.character,ip=excluded.ip,port=excluded.port,last_seen=excluded.last_seen",
		scan: func(r rowScanner) (string, peer, error) {
			var p peer
			err := r.Scan(&p.DeviceID, &p.Character, &p.IP, &p.Port, &p.LastSeen)
			return p.DeviceID, p, err
		}, values: func(id string, p peer) []any { return []any{id, p.Character, p.IP, p.Port, p.LastSeen} }}
}

func voiceStore(d *bridgeDB) *store[string] {
	return &store[string]{db: d.DB, table: "voices", key: "device_id", columns: "device_id,voice", upsert: "voice=excluded.voice",
		scan: scanString, values: func(id, voice string) []any { return []any{id, voice} }}
}

func scanString(r rowScanner) (string, string, error) {
	var id, value string
	err := r.Scan(&id, &value)
	return id, value, err
}

func sessionStore(d *bridgeDB) *store[string] {
	return &store[string]{db: d.DB, table: "sessions", key: "device_id", columns: "device_id,session_id,updated_at",
		upsert: "session_id=excluded.session_id,updated_at=excluded.updated_at",
		scan: func(r rowScanner) (string, string, error) {
			var id, sid string
			var at int64
			err := r.Scan(&id, &sid, &at)
			return id, sid, err
		}, values: func(id, sid string) []any { return []any{id, sid, time.Now().UnixMilli()} }}
}

func virtualStore(d *bridgeDB) *store[virtualClient] {
	return &store[virtualClient]{db: d.DB, table: "virtual_clients", key: "id", columns: "id,character,theme,voice,created_at",
		upsert: "character=excluded.character,theme=excluded.theme,voice=excluded.voice,created_at=excluded.created_at",
		scan: func(r rowScanner) (string, virtualClient, error) {
			var v virtualClient
			var at string
			err := r.Scan(&v.ID, &v.Character, &v.Theme, &v.Voice, &at)
			if err == nil {
				v.CreatedAt, err = time.Parse(time.RFC3339Nano, at)
			}
			return v.ID, v, err
		}, values: func(id string, v virtualClient) []any {
			return []any{id, v.Character, v.Theme, v.Voice, v.CreatedAt.Format(time.RFC3339Nano)}
		}}
}

func tunnelStore(d *bridgeDB) *store[tunnelRecord] {
	return &store[tunnelRecord]{db: d.DB, table: "tunnels", key: "device_id", columns: "device_id,route_name,url,local_port,scoped_token,profile,created_at",
		upsert: "route_name=excluded.route_name,url=excluded.url,local_port=excluded.local_port,scoped_token=excluded.scoped_token,profile=excluded.profile",
		scan: func(r rowScanner) (string, tunnelRecord, error) {
			var v tunnelRecord
			err := r.Scan(&v.DeviceID, &v.RouteName, &v.URL, &v.LocalPort, &v.Token, &v.Profile, &v.CreatedAt)
			return v.DeviceID, v, err
		}, values: func(id string, v tunnelRecord) []any {
			return []any{id, v.RouteName, v.URL, v.LocalPort, v.Token, v.Profile, v.CreatedAt}
		}}
}

func migrateMap[T any](ctx context.Context, tx *sql.Tx, dir string, s *store[T], valid func(string, T) bool) error {
	return legacyRows(dir, s.table, func(id string, raw json.RawMessage) error {
		var value T
		if json.Unmarshal(raw, &value) != nil || !valid(id, value) {
			return nil
		}
		if v, ok := any(value).(tunnelRecord); ok && v.CreatedAt == "" {
			v.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			value = any(v).(T)
		}
		args := s.values(id, value)
		_, err := tx.ExecContext(ctx, "INSERT INTO "+s.table+" ("+s.columns+") VALUES ("+placeholders(len(args))+") ON CONFLICT DO NOTHING", args...)
		return err
	})
}

func legacyRows(dir, name string, visit func(string, json.RawMessage) error) error {
	raw, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s.json: %w", name, err)
	}
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil || rows == nil {
		return fmt.Errorf("invalid %s.json", name)
	}
	for id, row := range rows {
		if deviceIDRE.MatchString(id) {
			if err := visit(id, row); err != nil {
				return fmt.Errorf("import %s.json: %w", name, err)
			}
		}
	}
	return nil
}

func (d *bridgeDB) migrate(ctx context.Context, legacy string) error {
	if legacy == "" {
		return nil
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var migrated string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='migrated_json_v1'").Scan(&migrated)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, deviceStore(d), validLegacyPeer); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, sessionStore(d), func(_ string, sid string) bool { return sessionIDRE.MatchString(sid) }); err != nil {
		return err
	}
	// Preserve alias rows as well as the canonical mapping. Prefer an existing LAN session.
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (device_id,session_id,updated_at)
		SELECT substr(device_id,7),session_id,updated_at FROM sessions
		WHERE device_id GLOB 'scope-virt-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]' ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, virtualStore(d), validLegacyVirtual); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, voiceStore(d), func(id, voice string) bool {
		return id == strings.ToLower(id) && voiceNames[voice] != ""
	}); err != nil {
		return err
	}
	if err := migrateMap(ctx, tx, legacy, tunnelStore(d), validLegacyTunnel); err != nil {
		return err
	}
	if err := legacyRows(legacy, "chat_events", func(id string, raw json.RawMessage) error {
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return nil
		}
		for _, item := range entries {
			var e chatEntry
			if json.Unmarshal(item, &e) == nil && e.ID != "" && !e.Timestamp.IsZero() {
				if _, err := tx.ExecContext(ctx, insertChat, chatValues(id, e)...); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO meta VALUES ('migrated_json_v1','1')"); err != nil {
		return err
	}
	return tx.Commit()
}
