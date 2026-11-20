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
	`CREATE TABLE IF NOT EXISTS device_prefs (device_id TEXT PRIMARY KEY, auto_volume INTEGER NOT NULL DEFAULT 1, manual_volume INTEGER NOT NULL DEFAULT 55)`,
	`CREATE TABLE IF NOT EXISTS cats (device_id TEXT PRIMARY KEY, name TEXT NOT NULL, configured INTEGER NOT NULL DEFAULT 0, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS saved_cats (slug TEXT PRIMARY KEY, name TEXT NOT NULL, face_bg TEXT NOT NULL, mark_color TEXT NOT NULL, accent_color TEXT NOT NULL, personality TEXT NOT NULL DEFAULT '', voice TEXT NOT NULL DEFAULT '', extras_json TEXT NOT NULL DEFAULT '[]', updated_at INTEGER NOT NULL, face_svg TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS tunnels (device_id TEXT PRIMARY KEY, route_name TEXT NOT NULL, url TEXT NOT NULL, local_port INTEGER NOT NULL, scoped_token TEXT NOT NULL, profile TEXT NOT NULL, created_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS chat_events (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, scene_title TEXT NOT NULL DEFAULT '', tool_summary TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS chat_device_ts ON chat_events (device_id, ts_ms)`,
	`CREATE TABLE IF NOT EXISTS messages (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, role TEXT NOT NULL, sender TEXT NOT NULL, text TEXT NOT NULL, source TEXT NOT NULL, card_title TEXT NOT NULL DEFAULT '', ts_ms INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS messages_device_ts ON messages (device_id, ts_ms)`,
	`CREATE TABLE IF NOT EXISTS memory (scope TEXT NOT NULL, key TEXT NOT NULL, value_json TEXT NOT NULL, version INTEGER NOT NULL DEFAULT 1, updated_at INTEGER NOT NULL, PRIMARY KEY (scope, key))`,
	`CREATE TABLE IF NOT EXISTS apps (name TEXT PRIMARY KEY, title TEXT NOT NULL, kind TEXT NOT NULL, svg TEXT NOT NULL DEFAULT '', code TEXT NOT NULL DEFAULT '', tick_ms INTEGER NOT NULL DEFAULT 0, max_ticks INTEGER NOT NULL DEFAULT 0, author TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS timers (id TEXT PRIMARY KEY, device_id TEXT NOT NULL, label TEXT NOT NULL, say TEXT NOT NULL, chime TEXT NOT NULL, wake_agent INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, fire_at TEXT NOT NULL)`,
}

type bridgeDB struct {
	*sql.DB
	cloud *turso.TursoSyncDb
}

func (d *bridgeDB) getMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := d.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return value, err
}

func (d *bridgeDB) setMeta(ctx context.Context, key, value string) error {
	_, err := d.ExecContext(ctx, `INSERT INTO meta VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
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
	if err := d.addV2Columns(ctx); err != nil {
		return nil, fmt.Errorf("migrate EchoEar v2 columns: %w", err)
	}
	if err := d.addTimerAppColumn(ctx); err != nil {
		return nil, fmt.Errorf("migrate EchoEar timer apps: %w", err)
	}
	if err := d.migrateFaceSVG(ctx); err != nil {
		return nil, fmt.Errorf("migrate EchoEar face SVG: %w", err)
	}
	if err := d.migrateLocalMessages(ctx); err != nil {
		return nil, fmt.Errorf("backfill EchoEar chat events: %w", err)
	}
	if err := d.migrateCats(ctx, deviceVoiceStore(d, fallbackVoice)); err != nil {
		return nil, fmt.Errorf("migrate EchoEar cats: %w", err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO device_prefs (device_id)
		SELECT lower(device_id) FROM cats UNION SELECT lower(device_id) FROM devices UNION SELECT lower(id) FROM virtual_clients WHERE true
		ON CONFLICT DO NOTHING`); err != nil {
		return nil, fmt.Errorf("seed EchoEar device preferences: %w", err)
	}
	return d, nil
}

func (d *bridgeDB) addV2Columns(ctx context.Context) error {
	return d.migrateColumns(ctx, "migrated_v2_columns",
		`ALTER TABLE cats ADD COLUMN svg_extras TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE saved_cats ADD COLUMN svg_extras TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE timers ADD COLUMN notes_json TEXT NOT NULL DEFAULT '[]'`)
}

func (d *bridgeDB) addTimerAppColumn(ctx context.Context) error {
	return d.migrateColumns(ctx, "migrated_timer_app",
		`ALTER TABLE timers ADD COLUMN app_name TEXT NOT NULL DEFAULT ''`)
}

func (d *bridgeDB) migrateColumns(ctx context.Context, marker string, statements ...string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, marker).Scan(&done)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, q := range statements {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta VALUES (?,'1')`, marker); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *bridgeDB) migrateFaceSVG(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []struct{ name, key, configured string }{
		{"cats", "device_id", "configured"}, {"saved_cats", "slug", "1"},
	} {
		rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table.name+`)`)
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var name string
			var cid, kind, notNull, defaultVal, pk any
			if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultVal, &pk); err != nil {
				rows.Close()
				return err
			}
			found = found || name == "face_svg"
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if !found {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table.name+` ADD COLUMN face_svg TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		rows, err = tx.QueryContext(ctx, `SELECT `+table.key+`,face_bg,mark_color,accent_color,`+table.configured+
			` FROM `+table.name+` WHERE face_svg=''`)
		if err != nil {
			return err
		}
		type seed struct{ key, svg string }
		var seeds []seed
		for rows.Next() {
			var key, bg, mark, accent string
			var configured int
			if err := rows.Scan(&key, &bg, &mark, &accent, &configured); err != nil {
				rows.Close()
				return err
			}
			seeds = append(seeds, seed{key, defaultFaceSVG(bg, mark, accent, configured != 0)})
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, s := range seeds {
			if _, err := tx.ExecContext(ctx, `UPDATE `+table.name+` SET face_svg=? WHERE `+table.key+`=?`, s.svg, s.key); err != nil {
				return err
			}
		}
		// Overlays could mask the editable eyes (Cake's closed-eye overlay did).
		if _, err := tx.ExecContext(ctx, `UPDATE `+table.name+` SET svg_extras='', extras_json='[]'
			WHERE (svg_extras<>'' OR extras_json<>'[]') AND NOT EXISTS (SELECT 1 FROM meta WHERE key='migrated_face_svg_v1')`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta VALUES ('migrated_face_svg_v1','1') ON CONFLICT DO NOTHING`); err != nil {
		return err
	}
	return tx.Commit()
}

const initialAutoVolume = 55

func (b *bridge) volumePrefs(id string) (auto bool, level int) {
	auto, level = true, initialAutoVolume
	if b.db != nil {
		err := b.db.QueryRow(`SELECT auto_volume,manual_volume FROM device_prefs WHERE device_id=?`, strings.ToLower(id)).Scan(&auto, &level)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Print("EchoEar could not read volume preferences")
		}
	}
	return auto, level
}

func (b *bridge) saveVolumePrefs(ctx context.Context, id string, auto bool, level int) error {
	if b.db == nil {
		return problem(503, "Volume storage unavailable")
	}
	_, err := b.db.ExecContext(ctx, `INSERT INTO device_prefs (device_id,auto_volume,manual_volume) VALUES (?,?,?)
		ON CONFLICT(device_id) DO UPDATE SET auto_volume=excluded.auto_volume,manual_volume=excluded.manual_volume`,
		strings.ToLower(id), auto, level)
	if err != nil {
		return problem(502, "Cannot save volume")
	}
	return nil
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
	return s.allContext(context.Background())
}

func (s *store[T]) allContext(ctx context.Context) (map[string]T, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+s.columns+" FROM "+s.table)
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
	return s.getContext(context.Background(), id)
}

func (s *store[T]) getContext(ctx context.Context, id string) (T, bool, error) {
	_, value, err := s.scan(s.db.QueryRowContext(ctx, "SELECT "+s.columns+" FROM "+s.table+" WHERE "+s.key+"=?", id))
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
		return id == strings.ToLower(id) && voiceName(voice) != ""
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
				var fields struct {
					SceneTitle  string `json:"scene_title"`
					ToolSummary string `json:"tool_summary"`
				}
				if json.Unmarshal(item, &fields) == nil {
					if e.SceneTitle == "" {
						e.SceneTitle = fields.SceneTitle
					}
					e.ToolSummary = fields.ToolSummary
				}
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
