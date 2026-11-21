package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strconv"
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

type historyCursor struct {
	TS int64  `json:"before_ts"`
	ID string `json:"before_id"`
}

type historyPage struct {
	DeviceID string         `json:"device_id"`
	Messages []chatEntry    `json:"messages"`
	HasMore  bool           `json:"has_more"`
	Next     *historyCursor `json:"next_cursor,omitempty"`
}

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
	return h.readBefore(ctx, id, limit, nil)
}

func (h *chatHistory) readBefore(ctx context.Context, id string, limit int, before *historyCursor) ([]chatEntry, error) {
	query := `SELECT id,role,sender,text,source,card_title,ts_ms FROM messages WHERE device_id=?`
	args := []any{strings.ToLower(id)}
	if before != nil {
		query += ` AND ts_ms<=? AND (ts_ms<? OR id<?)`
		args = append(args, before.TS, before.TS, before.ID)
	}
	if limit > 0 {
		query += ` ORDER BY ts_ms DESC,id DESC LIMIT ?`
		args = append(args, limit)
	} else {
		query += ` ORDER BY ts_ms,id`
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

func (b *bridge) messages(r *http.Request, id string) (any, error) {
	if !deviceIDRE.MatchString(id) {
		return nil, problem(400, "Invalid device ID")
	}
	query := r.URL.Query()
	limit := 100
	if query.Has("limit") {
		n, err := strconv.Atoi(query.Get("limit"))
		if err != nil || n < 1 {
			return nil, problem(400, "Expected a positive history limit")
		}
		limit = min(n, 500)
	}
	var before *historyCursor
	if query.Has("before_ts") || query.Has("before_id") {
		ts, err := strconv.ParseInt(query.Get("before_ts"), 10, 64)
		key := query.Get("before_id")
		if err != nil || key == "" || len(key) > 512 {
			return nil, problem(400, "Expected before_ts (Unix milliseconds) and before_id")
		}
		before = &historyCursor{TS: ts, ID: key}
	}
	return b.readHistoryPage(r.Context(), id, limit, before)
}

func (b *bridge) readHistoryPage(ctx context.Context, id string, limit int, before *historyCursor) (historyPage, error) {
	out := historyPage{DeviceID: id, Messages: []chatEntry{}}
	if b.history == nil {
		return out, nil
	}
	entries, err := b.history.readBefore(ctx, id, limit+1, before)
	if err != nil {
		return out, err
	}
	out.HasMore = len(entries) > limit
	if out.HasMore {
		entries = entries[1:] // The extra row is the oldest.
		out.Next = &historyCursor{TS: entries[0].Timestamp.UnixMilli(), ID: entries[0].ID}
	}
	out.Messages = entries
	return out, nil
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
			if d.callState != callNone {
				b.ws.mu.Unlock()
				return nil, problem(409, "Cat is in a call")
			}
			b.ws.startTurn(d, time.Now(), false, "typed", func(ctx context.Context, turn uint64) {
				if scope != "" || r.Context().Value(photoScopeContext{}) != nil {
					ctx = withoutPhotoAccess(ctx)
				}
				b.ws.runTurn(ctx, d, turn, text, "typed", nil)
			})
			b.ws.mu.Unlock()
			return object{"ok": true, "device_id": sender.DeviceID}, nil
		}
		b.ws.mu.Unlock()
	}
	if scope != "" {
		ctx = withoutPhotoAccess(ctx)
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
		b.catMu.Lock()
		defer b.catMu.Unlock()
		cat, profilesChanged, err := b.changeCat(ctx, who, b.voiceTheme(who), "reset_blank", nil)
		if err != nil {
			return err
		}
		if err := b.oc.save(who.DeviceID, ""); err != nil {
			return err
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			defer b.ws.mu.Unlock()
			if d := b.ws.devices[strings.ToLower(who.DeviceID)]; d != nil {
				b.ws.stopCatTurn(d)
			}
			b.ws.catChanged(who.DeviceID, cat, profilesChanged)
		}
		return nil
	})
}
