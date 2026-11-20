package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	timerGrace  = 2 * time.Second
	timerExpiry = 2 * time.Minute
)

var timerIDRE = regexp.MustCompile(`^[a-f0-9]{24}$`)

type savedTimer struct {
	ID        string    `json:"id"`
	DeviceID  string    `json:"device_id"`
	Label     string    `json:"label"`
	Say       string    `json:"say"`
	Chime     string    `json:"chime"`
	WakeAgent bool      `json:"wake_agent"`
	CreatedAt time.Time `json:"created_at"`
	FireAt    time.Time `json:"fire_at"`
}

type timerManager struct {
	b       *bridge
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	handles map[string]*time.Timer
	wg      sync.WaitGroup
	closed  bool
	// HTTP MCP calls do not carry a trusted OpenCode session ID. Reject
	// wake-agent scheduling bridge-wide during any timer-triggered turn.
	fromTimerTurn int
}

const timerColumns = "id,device_id,label,say,chime,wake_agent,created_at,fire_at"

func newTimerManager(b *bridge) *timerManager {
	ctx, cancel := context.WithCancel(b.root)
	return &timerManager{b: b, ctx: ctx, cancel: cancel, handles: make(map[string]*time.Timer)}
}

func scanTimer(r rowScanner) (savedTimer, error) {
	var t savedTimer
	var wake int
	var created, fire string
	err := r.Scan(&t.ID, &t.DeviceID, &t.Label, &t.Say, &t.Chime, &wake, &created, &fire)
	if err != nil {
		return t, err
	}
	t.WakeAgent = wake != 0
	t.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err == nil {
		t.FireAt, err = time.Parse(time.RFC3339Nano, fire)
	}
	return t, err
}

func (m *timerManager) list(ctx context.Context, id string) ([]savedTimer, error) {
	rows, err := m.b.db.QueryContext(ctx, "SELECT "+timerColumns+" FROM timers WHERE (?='' OR device_id=?)", id, id)
	if err != nil {
		return nil, problem(502, "Cannot read timers")
	}
	defer rows.Close()
	out := []savedTimer{}
	for rows.Next() {
		t, err := scanTimer(rows)
		if err != nil {
			return nil, problem(502, "Cannot read timers")
		}
		out = append(out, t)
	}
	if rows.Err() != nil {
		return nil, problem(502, "Cannot read timers")
	}
	slices.SortFunc(out, func(a, b savedTimer) int { return a.FireAt.Compare(b.FireAt) })
	return out, nil
}

func (m *timerManager) restore(now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	timers, err := m.list(m.ctx, "")
	if err != nil {
		return err
	}
	for _, t := range timers {
		delay := t.FireAt.Sub(now)
		switch {
		case delay < -timerExpiry:
			if _, err := deleteRow(m.ctx, m.b.db, "DELETE FROM timers WHERE id=?", t.ID); err != nil {
				return problem(502, "Cannot prune expired timers")
			}
		case delay <= 0:
			m.armLocked(t.ID, timerGrace)
		default:
			m.armLocked(t.ID, delay)
		}
	}
	return nil
}

func parseTimer(args object, now time.Time) (savedTimer, error) {
	t := savedTimer{DeviceID: str(args["device_id"]), CreatedAt: now.UTC(), Label: "Timer", Chime: "celebrate"}
	seconds, err := mcpUint(args, "in_seconds")
	at, atSet := args["at"]
	if err != nil || atSet == (seconds != nil) {
		return t, problem(400, "Provide exactly one of in_seconds or at")
	}
	if seconds != nil {
		if *seconds < 1 || *seconds > 86400 {
			return t, problem(400, "in_seconds must be 1–86400")
		}
		t.FireAt = now.Add(time.Duration(*seconds) * time.Second).UTC()
	} else {
		t.FireAt, err = time.Parse(time.RFC3339, str(at))
		delay := t.FireAt.Sub(now)
		if err != nil || delay <= 0 || delay > 24*time.Hour {
			return t, problem(400, "at must be RFC3339 within the next 24 hours")
		}
		t.FireAt = t.FireAt.UTC()
	}
	if v, ok := args["wake_agent"]; ok {
		if t.WakeAgent, ok = v.(bool); !ok {
			return t, problem(400, "Expected boolean wake_agent")
		}
	}
	if t.WakeAgent && (seconds == nil || *seconds < 30) {
		return t, problem(400, "wake_agent requires in_seconds of at least 30")
	}
	if label := shortText(args["label"], 60, true); label != "" {
		t.Label = label
	}
	if _, present := args["say"]; present {
		t.Say, err = spokenText(str(args["say"]))
		if err != nil {
			return t, err
		}
	}
	if _, present := args["chime"]; present {
		t.Chime = str(args["chime"])
		if !validChime(t.Chime) {
			return t, problem(400, "Unknown chime kind")
		}
	}
	t.ID = randomHex(12)
	return t, nil
}

func (m *timerManager) set(ctx context.Context, t savedTimer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return problem(503, "Timers unavailable")
	}
	if t.WakeAgent && m.fromTimerTurn != 0 {
		return problem(409, "Cannot schedule a wake-agent timer during a timer-triggered turn")
	}
	result, err := m.b.db.ExecContext(ctx, `INSERT INTO timers (`+timerColumns+`)
		SELECT ?,?,?,?,?,?,?,? WHERE (SELECT count(*) FROM timers WHERE device_id=?) < 8
		AND (SELECT count(*) FROM timers) < 32`, t.ID, t.DeviceID, t.Label, t.Say, t.Chime, t.WakeAgent,
		t.CreatedAt.Format(time.RFC3339Nano), t.FireAt.Format(time.RFC3339Nano), t.DeviceID)
	if err != nil {
		return problem(502, "Cannot save timer")
	}
	n, err := result.RowsAffected()
	if err != nil {
		return problem(502, "Cannot save timer")
	}
	if n == 0 {
		return problem(409, "Timer limit reached (8 per device, 32 total)")
	}
	m.armLocked(t.ID, time.Until(t.FireAt))
	return nil
}

func (m *timerManager) armLocked(id string, delay time.Duration) {
	m.wg.Add(1)
	m.handles[id] = time.AfterFunc(delay, func() {
		defer m.wg.Done()
		m.fire(id)
	})
}

func (m *timerManager) stopLocked(id string) {
	if handle := m.handles[id]; handle != nil {
		if handle.Stop() {
			m.wg.Done()
		}
		delete(m.handles, id)
	}
}

func (m *timerManager) close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	for id := range m.handles {
		m.stopLocked(id)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *timerManager) claim(id string) (savedTimer, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.handles[id] == nil {
		return savedTimer{}, false
	}
	m.stopLocked(id)
	t, err := scanTimer(m.b.db.QueryRowContext(m.ctx, "DELETE FROM timers WHERE id=? RETURNING "+timerColumns, id))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) && m.ctx.Err() == nil {
			log.Print("EchoEar could not claim timer")
		}
		return savedTimer{}, false
	}
	if t.WakeAgent {
		m.fromTimerTurn++
	}
	return t, true
}

func (m *timerManager) fire(id string) {
	t, ok := m.claim(id)
	if !ok {
		return
	}
	if t.WakeAgent {
		defer func() {
			m.mu.Lock()
			m.fromTimerTurn--
			m.mu.Unlock()
		}()
	}
	m.b.timerUpdate(t.DeviceID)
	m.b.deliverTimer(m.ctx, t)
}

func (m *timerManager) cancelTimers(ctx context.Context, device, id string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, problem(503, "Timers unavailable")
	}
	ids := []string{}
	if id != "" {
		var owner string
		err := m.b.db.QueryRowContext(ctx, "SELECT device_id FROM timers WHERE id=?", id).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, problem(404, "Timer not found")
		}
		if err != nil {
			return nil, problem(502, "Cannot read timer")
		}
		if device != "" && device != owner {
			return nil, problem(403, "Device out of scope")
		}
		deleted, err := deleteRow(ctx, m.b.db, "DELETE FROM timers WHERE id=? AND device_id=?", id, owner)
		if err != nil {
			return nil, problem(502, "Cannot cancel timer")
		}
		if !deleted {
			return nil, problem(404, "Timer not found")
		}
		m.stopLocked(id)
		ids = append(ids, owner)
	} else {
		rows, err := m.b.db.QueryContext(ctx, "DELETE FROM timers WHERE device_id=? RETURNING id", device)
		if err != nil {
			return nil, problem(502, "Cannot cancel timers")
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, problem(502, "Cannot cancel timers")
			}
			m.stopLocked(id)
		}
		if rows.Err() != nil {
			return nil, problem(502, "Cannot cancel timers")
		}
		ids = append(ids, device)
	}
	return ids, nil
}

func (b *bridge) timerUpdate(id string) {
	if h := b.ws; h != nil {
		h.mu.Lock()
		defer h.mu.Unlock()
		key := strings.ToLower(id)
		frame := textFrame(object{"type": "timers", "device_id": id})
		for c := range h.clients {
			if c.watch == key || c.device != nil && c.device.key == key && c.device.virtual() {
				c.enqueue(frame)
			}
		}
	}
}

func (b *bridge) deliverTimer(ctx context.Context, t savedTimer) {
	who, err := b.lookupDevice(t.DeviceID)
	if err != nil || ctx.Err() != nil {
		return
	}
	message := t.Label
	if t.Say != "" {
		message += ": " + t.Say
	}
	if !t.WakeAgent {
		b.chat(t.DeviceID, newChat("system", who.Character, "Time's up! "+message, "timer"), true)
	}
	h := b.ws
	h.mu.Lock()
	d := h.devices[strings.ToLower(t.DeviceID)]
	if d != nil && h.active(d) {
		now := time.Now()
		if t.WakeAgent {
			completed := make(chan struct{})
			h.startTurn(d, now, true, func(turnCtx context.Context, id uint64) {
				defer close(completed)
				turnCtx, cancel := context.WithCancel(turnCtx)
				defer cancel()
				stop := context.AfterFunc(ctx, cancel)
				defer stop()
				h.runTurn(turnCtx, d, id, "Timer fired: "+message, "timer", nil)
			})
			h.mu.Unlock()
			<-completed
			return
		}
		h.wake(d, now)
		h.chime(d, t.Chime)
		view := noticeScene("Time's up!", message)
		view.nodes[len(view.nodes)-1]["s"] = "OK"
		view.actions["cat"] = sceneAction{"dismiss", ""}
		d.card, d.drawer = &view, false
		if t.Say != "" {
			h.speakLocal(d, t.Say)
		}
		h.render(d, now, false)
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	if t.WakeAgent && b.oc != nil {
		ctx, cancel := context.WithTimeout(ctx, turnTimeout)
		defer cancel()
		_, _ = b.generateReply(ctx, ctx, who, "Timer fired: "+message, turnOptions{source: "timer"})
	} else if who.Port != 0 && !t.WakeAgent {
		_, _ = b.mcpChime(ctx, who, t.Chime)
		if t.Say != "" {
			_, _ = b.speakMCP(ctx, who, t.Say)
		}
	}
}

func (b *bridge) timerMCP(ctx context.Context, args object) (object, error) {
	m := b.timers
	if m == nil {
		return nil, problem(503, "Timers unavailable")
	}
	id := str(args["device_id"])
	if !deviceIDRE.MatchString(id) {
		return nil, problem(400, "Invalid timer device_id")
	}
	switch str(args["op"]) {
	case "set":
		if _, err := b.lookupDevice(id); err != nil {
			return nil, err
		}
		t, err := parseTimer(args, time.Now())
		if err != nil {
			return nil, err
		}
		if err := m.set(ctx, t); err != nil {
			return nil, err
		}
		b.timerUpdate(id)
		return object{"ok": true, "id": t.ID, "timer": t}, nil
	case "list":
		timers, err := m.list(ctx, id)
		return object{"timers": timers}, err
	case "cancel":
		timerID, all := str(args["id"]), args["all"] == true
		if all && timerID != "" || !all && !timerIDRE.MatchString(timerID) {
			return nil, problem(400, "Provide exactly one of id or all=true")
		}
		if _, err := m.cancelTimers(ctx, id, timerID); err != nil {
			return nil, err
		}
		b.timerUpdate(id)
		return object{"ok": true}, nil
	}
	return nil, problem(400, "Unknown timer operation")
}

func (b *bridge) timerAPI(w http.ResponseWriter, r *http.Request, path, scope string) (any, error) {
	m := b.timers
	if m == nil {
		return nil, problem(503, "Timers unavailable")
	}
	id := r.URL.Query().Get("device_id")
	if scope != "" {
		if id != "" && id != scope {
			return nil, problem(403, "Device out of scope")
		}
		id = scope
	}
	if path == "/api/timers" && r.Method == http.MethodGet {
		if id != "" && !deviceIDRE.MatchString(id) {
			return nil, problem(400, "Invalid timer device_id")
		}
		timers, err := m.list(r.Context(), id)
		return object{"timers": timers}, err
	}
	if timerID, ok := strings.CutPrefix(path, "/api/timers/"); ok && r.Method == http.MethodDelete && timerIDRE.MatchString(timerID) {
		ids, err := m.cancelTimers(r.Context(), scope, timerID)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			b.timerUpdate(id)
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}
