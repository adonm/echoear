package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const localChatLimit = 200

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

type chatHistory struct {
	mu      sync.Mutex
	path    string
	entries map[string][]chatEntry
}

func loadChatHistory(path string) *chatHistory {
	h := &chatHistory{path: path, entries: make(map[string][]chatEntry)}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &h.entries); err != nil || h.entries == nil {
			log.Print("EchoEar chat event file is invalid; using in-memory history")
			h.entries, h.path = make(map[string][]chatEntry), ""
		}
		for id, entries := range h.entries {
			h.entries[id] = entries[max(0, len(entries)-localChatLimit):]
		}
	} else if !os.IsNotExist(err) {
		log.Print("EchoEar chat event file is unreadable; using in-memory history")
		h.path = ""
	}
	return h
}

func (h *chatHistory) add(id string, entry chatEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	all := append(h.entries[id], entry)
	h.entries[id] = all[max(0, len(all)-localChatLimit):]
	if h.path != "" && writeMap(h.path, h.entries) != nil {
		log.Print("EchoEar could not persist chat events")
	}
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
		b.history.mu.Lock()
		for _, entry := range b.history.entries[id] {
			if !seen[entry.ID] {
				entries = append(entries, entry)
				seen[entry.ID] = true
			}
		}
		b.history.mu.Unlock()
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
		if b.ws != nil {
			b.ws.mu.Lock()
			if d := b.ws.devices[strings.ToLower(id)]; d != nil {
				b.ws.cancelLocalTurn(d)
				b.ws.stopPlayback(d, true)
				b.ws.clearView(d)
				b.ws.render(d, time.Now(), false)
			}
			b.ws.mu.Unlock()
		}
		return object{"ok": true}, b.oc.reset(ctx, id)
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
