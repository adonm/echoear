package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

const localChatLimit = 200

func stripEchoEarHeader(text string) string {
	first, rest, ok := strings.Cut(text, "\n")
	if ok && strings.HasPrefix(first, "[EchoEar: ") && strings.HasSuffix(strings.TrimSuffix(first, "\r"), "]") {
		return rest
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
	SceneTitle  string    `json:"scene_title,omitempty"`
	ToolSummary string    `json:"tool_summary,omitempty"`
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

func (h *chatHistory) add(id string, e chatEntry) {
	if _, err := h.db.Exec(insertChat, chatValues(id, e)...); err != nil {
		log.Print("EchoEar could not persist chat events")
	}
}

func (h *chatHistory) list(ctx context.Context, id string) ([]chatEntry, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT id,role,sender,text,source,scene_title,tool_summary,ts_ms
		FROM chat_events WHERE device_id=? ORDER BY ts_ms DESC,rowid DESC LIMIT ?`, id, localChatLimit)
	if err != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	defer rows.Close()
	var entries []chatEntry
	for rows.Next() {
		var e chatEntry
		var ts int64
		if err := rows.Scan(&e.ID, &e.Role, &e.Sender, &e.Text, &e.Source, &e.SceneTitle, &e.ToolSummary, &ts); err != nil {
			return nil, problem(502, "Cannot read chat events")
		}
		e.Timestamp = time.UnixMilli(ts).UTC()
		entries = append(entries, e)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read chat events")
	}
	slices.Reverse(entries)
	return entries, nil
}

func (b *bridge) chat(id string, entry chatEntry, persist bool) {
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
	who, err := b.lookupDevice(id)
	if err != nil {
		return nil, err
	}
	entries := []chatEntry{}
	seen := make(map[string]bool)
	if b.oc != nil {
		activeSID, err := b.oc.get(id)
		if err != nil {
			return nil, err
		}
		sessions, err := b.oc.ListDeviceSessions(ctx, id, activeSID)
		if err != nil {
			return nil, err
		}
		for _, sid := range sessions {
			messages, err := b.oc.GetSessionMessages(ctx, sid)
			if err != nil {
				var status *upstreamStatus
				if errors.As(err, &status) && status.code == http.StatusNotFound {
					continue // The session was removed after it was listed.
				}
				return nil, err
			}
			unique := messages[:0]
			for _, message := range messages {
				if seen[message.ID] {
					continue
				}
				seen[message.ID] = true
				if message.Role == "assistant" {
					message.Sender = who.Character
				}
				unique = append(unique, message)
			}
			if len(unique) == 0 {
				continue
			}
			if len(entries) > 0 {
				divider := chatEntry{ID: "divider-" + sid, Role: "system", Sender: "System",
					Text: "Started a new chat", Source: "system", Timestamp: unique[0].Timestamp}
				entries = append(entries, divider)
				seen[divider.ID] = true
			}
			entries = append(entries, unique...)
		}
	}
	if b.history != nil {
		events, err := b.history.list(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, entry := range events {
			if !seen[entry.ID] {
				entries = append(entries, entry)
				seen[entry.ID] = true
			}
		}
	}
	slices.SortStableFunc(entries, func(a, b chatEntry) int { return a.Timestamp.Compare(b.Timestamp) })
	return object{"device_id": id, "messages": entries}, nil
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
			if !b.ws.emit(sender.DeviceID, "speech", object{"text": text, "source": "text"}) {
				b.ws.startTurn(d, time.Now(), false, func(ctx context.Context, turn uint64) {
					b.ws.runTurn(ctx, d, turn, text, "typed", nil)
				})
			}
			b.ws.mu.Unlock()
			return object{"ok": true, "device_id": sender.DeviceID}, nil
		}
		b.ws.mu.Unlock()
	}
	out, err := b.turn(ctx, sender, text, nil, "")
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
