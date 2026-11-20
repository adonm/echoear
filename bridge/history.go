package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

func stripEchoEarHeader(text string) string {
	first, rest, ok := strings.Cut(text, "\n")
	if ok && strings.HasPrefix(first, "[") && strings.HasSuffix(strings.TrimSuffix(first, "\r"), "]") {
		if strings.HasPrefix(first, "[EchoEar: ") || strings.Contains(first, ") · hardware · ") || strings.Contains(first, ") · web · ") {
			return rest
		}
	}
	return text
}

type chatEntry struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Sender      string    `json:"sender"`
	Text        string    `json:"text"`
	Timestamp   time.Time `json:"timestamp"`
	Source      string    `json:"source"`
	SceneTitle  string    `json:"card_title,omitempty"`
	ToolSummary string    `json:"-"`
}

func newChat(role, sender, text, source string) chatEntry {
	return chatEntry{ID: "bridge-" + randomHex(12), Role: role, Sender: sender, Text: text,
		Source: source, Timestamp: time.Now().UTC()}
}

type chatHistory struct{ db *bridgeDB }

const insertChat = `INSERT INTO chat_events (id,device_id,role,sender,text,source,scene_title,tool_summary,ts_ms)
	VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`

func chatValues(id string, e chatEntry) []any {
	return []any{e.ID, id, e.Role, e.Sender, e.Text, e.Source, e.SceneTitle, e.ToolSummary, e.Timestamp.UnixMilli()}
}

const insertMessage = `INSERT INTO messages (id,device_id,role,sender,text,source,card_title,ts_ms)
	VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`

func canonicalMessage(e chatEntry) (chatEntry, bool) {
	e.Text = strings.TrimSpace(stripEchoEarHeader(e.Text))
	switch e.Role {
	case "user":
		if e.Source == "timer" {
			return e, false
		}
	case "assistant", "cat":
		e.Role = "cat"
		e.Text = strings.TrimSpace(fenced.ReplaceAllString(e.Text, ""))
	case "intercom", "card":
	case "system":
		e.Role = "card"
		if e.Source != "timer" {
			if e.SceneTitle == "" || e.Source != "mcp" {
				return e, false
			}
			e.Text = "Drew: " + e.SceneTitle
		}
	default:
		return e, false
	}
	if e.Role != "card" {
		e.SceneTitle = ""
	}
	e.ToolSummary = ""
	return e, e.ID != "" && e.Text != ""
}

func messageValues(id string, e chatEntry) []any {
	return []any{e.ID, strings.ToLower(id), e.Role, e.Sender, e.Text, e.Source, e.SceneTitle, e.Timestamp.UnixMilli()}
}

func (h *chatHistory) add(id string, e chatEntry) {
	if e, ok := canonicalMessage(e); ok {
		if _, err := h.db.Exec(insertMessage, messageValues(id, e)...); err != nil {
			log.Printf("EchoEar could not persist message: %v", err)
		}
	}
}

func (h *chatHistory) list(ctx context.Context, id string) ([]chatEntry, error) {
	return h.read(ctx, id, 0)
}

func (h *chatHistory) read(ctx context.Context, id string, limit int) ([]chatEntry, error) {
	query := `SELECT id,role,sender,text,source,card_title,ts_ms FROM messages WHERE device_id=?`
	args := []any{strings.ToLower(id)}
	if limit > 0 {
		query += ` ORDER BY ts_ms DESC,rowid DESC LIMIT ?`
		args = append(args, limit)
	} else {
		query += ` ORDER BY ts_ms,rowid`
	}
	rows, err := h.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	defer rows.Close()
	entries := []chatEntry{}
	for rows.Next() {
		var e chatEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.SceneTitle, &ts); err != nil {
			return nil, problem(502, "Cannot read chat events")
		}
		e.Timestamp = time.UnixMilli(ts).UTC()
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	if limit > 0 {
		slices.Reverse(entries)
	}
	return entries, nil
}

func (b *bridge) chat(id string, entry chatEntry, persist bool) {
	entry, ok := canonicalMessage(entry)
	if !ok {
		return
	}
	if persist && b.history != nil {
		b.history.add(id, entry)
	}
	if b.ws != nil {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		f := textFrame(object{"type": "chat", "device_id": id, "entry": entry})
		key := strings.ToLower(id)
		for c := range b.ws.clients {
			if c.watch == key || c.device != nil && c.device.key == key && c.device.virtual() {
				c.enqueue(f)
			}
		}
	}
}

func messagesDevice(path string) (string, bool) {
	suffix, ok := strings.CutPrefix(path, "/api/devices/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(suffix, "/messages")
	return id, ok && deviceIDRE.MatchString(id)
}

func (b *bridge) messages(ctx context.Context, id string) (any, error) {
	if b.history == nil {
		return object{"device_id": id, "messages": []chatEntry{}}, nil
	}
	entries, err := b.history.list(ctx, id)
	if err != nil {
		return nil, err
	}
	return object{"device_id": id, "messages": entries}, nil
}

type backfillMessage struct {
	id    string
	entry chatEntry
}

func (d *bridgeDB) migrateLocalMessages(ctx context.Context) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var done string
	err = tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='migrated_messages_v2_local'`).Scan(&done)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT device_id,id,role,sender,text,source,scene_title,ts_ms FROM chat_events`)
	if err != nil {
		return err
	}
	var batch []backfillMessage
	for rows.Next() {
		var row backfillMessage
		e := &row.entry
		var ts int64
		if err := rows.Scan(&row.id, &e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.SceneTitle, &ts); err != nil {
			rows.Close()
			return err
		}
		e.Timestamp = time.UnixMilli(ts)
		batch = append(batch, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return insertBackfill(ctx, tx, batch, "migrated_messages_v2_local")
}

func insertBackfill(ctx context.Context, tx *sql.Tx, batch []backfillMessage, marker string) error {
	for _, row := range batch {
		if e, ok := canonicalMessage(row.entry); ok {
			if _, err := tx.ExecContext(ctx, insertMessage, messageValues(row.id, e)...); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta VALUES (?, '1') ON CONFLICT DO NOTHING`, marker); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *bridgeDB) historyDeviceIDs(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT lower(device_id) FROM devices
		UNION SELECT lower(id) FROM virtual_clients
		UNION SELECT lower(device_id) FROM cats
		UNION SELECT lower(CASE WHEN device_id LIKE 'scope-virt-%' THEN substr(device_id,7) ELSE device_id END) FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if deviceIDRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (b *bridge) migrateSessionMessages(ctx context.Context) error {
	const marker = "migrated_messages_v2"
	var done string
	err := b.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, marker).Scan(&done)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	ids, err := b.db.historyDeviceIDs(ctx)
	if err != nil {
		return err
	}
	var batch []backfillMessage
	for _, id := range ids {
		active, err := b.oc.get(id)
		if err != nil {
			return err
		}
		alias, err := b.oc.get("scope-" + id)
		if err != nil {
			return err
		}
		sessions, err := b.oc.listDeviceSessions(ctx, id, active, false)
		if err != nil {
			return err
		}
		if alias != "" && !slices.Contains(sessions, alias) {
			sessions = append(sessions, alias)
		}
		who, err := b.lookupDevice(id)
		if err != nil {
			who = peer{DeviceID: id, Character: "EchoEar"}
		}
		for _, sid := range sessions {
			entries, err := b.oc.GetSessionMessages(ctx, sid)
			var status *upstreamStatus
			if errors.As(err, &status) && status.code == http.StatusNotFound {
				continue
			}
			if err != nil {
				return err
			}
			for _, e := range entries {
				if e.Role != "user" && e.Role != "assistant" {
					continue
				}
				if e.Role == "assistant" {
					e.Sender = who.Character
				}
				batch = append(batch, backfillMessage{id, e})
			}
		}
	}
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return insertBackfill(ctx, tx, batch, marker)
}

func (b *bridge) chatAPI(r *http.Request, path, scope string) (any, error) {
	obj, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	if scope != "" && str(obj["device_id"]) != scope {
		return nil, problem(403, "Device out of scope")
	}
	id, err := deviceID(r, obj, path == "/api/chat")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), turnTimeout)
	defer cancel()
	if b.oc == nil {
		return nil, problem(503, "Chat unavailable")
	}
	if path == "/api/reset" {
		who, err := b.lookupDevice(id)
		var status *apiError
		if errors.As(err, &status) && status.status == 404 && !virtualID.MatchString(strings.ToLower(id)) {
			// Legacy HTTP-only sessions can exist without a registered device.
			return object{"ok": true}, b.oc.reset(ctx, id)
		}
		if err != nil {
			return nil, err
		}
		return object{"ok": true}, b.startNewCatSession(ctx, who)
	}
	text := strings.TrimSpace(str(obj["text"]))
	if text == "" || len(text) > 4096 {
		return nil, problem(400, "Expected nonempty text of at most 4096 bytes")
	}
	sender, err := b.lookupDevice(id)
	if err != nil {
		return nil, err
	}
	if b.ws != nil {
		b.ws.mu.Lock()
		d := b.ws.devices[strings.ToLower(id)]
		if d != nil && b.ws.active(d) {
			b.ws.startTurn(d, time.Now(), false, func(ctx context.Context, turn uint64) {
				b.ws.runTurn(ctx, d, turn, text, "typed", nil)
			})
			b.ws.mu.Unlock()
			return object{"ok": true, "device_id": sender.DeviceID}, nil
		}
		b.ws.mu.Unlock()
	}
	out, err := b.turn(ctx, sender, text)
	if err != nil {
		return nil, err
	}
	result := object{"ok": true, "device_id": sender.DeviceID, "reply": out.Reply, "session_id": out.SessionID}
	if scope == "" {
		result["audio_url"] = out.AudioURL
	}
	return result, nil
}

func (b *bridge) startNewCatSession(ctx context.Context, who peer) error {
	run := b.oc.prepareMappingChange(who.DeviceID)
	if b.ws != nil {
		b.ws.stopCatTurns(who.DeviceID)
	}
	return run(ctx, func() error {
		if b.ws != nil {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
		}
		cat, profilesChanged, err := b.changeCat(ctx, who, b.voiceTheme(who), "reset_blank", nil)
		if err != nil {
			return err
		}
		if err := b.oc.save(who.DeviceID, ""); err != nil {
			return err
		}
		if b.ws != nil {
			if d := b.ws.devices[strings.ToLower(who.DeviceID)]; d != nil {
				b.ws.stopCatTurn(d)
			}
			b.ws.catChanged(who.DeviceID, cat, profilesChanged)
		}
		return nil
	})
}
