package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const turnTimeout = 90 * time.Second

type ocEventData struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	MessageID          string `json:"messageID"`
	PartID             string `json:"partID"`
	Ordinal            int    `json:"ordinal"`
	Field              string `json:"field"`
	Delta              string `json:"delta"`
	Text               string `json:"text"`
	Status             struct {
		Type string `json:"type"`
	} `json:"status"`
	Part struct {
		ID        string `json:"id"`
		SessionID string `json:"sessionID"`
		MessageID string `json:"messageID"`
		Type      string `json:"type"`
		Text      string `json:"text"`
	} `json:"part"`
}

type ocEvent struct {
	Type       string      `json:"type"`
	Data       ocEventData `json:"data"`
	Properties ocEventData `json:"properties"` // Older event envelope.
}

type eventFeed struct {
	mu        sync.Mutex
	ready     chan struct{}
	connected bool
	bySession map[string]map[chan ocEvent]struct{}
}

func (f *eventFeed) setConnected(connected bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = connected
	if connected {
		close(f.ready)
	} else {
		f.ready = make(chan struct{})
		for _, subs := range f.bySession {
			for ch := range subs {
				close(ch)
			}
		}
		clear(f.bySession)
	}
}

func (f *eventFeed) subscribe(ctx context.Context, sid string) (<-chan ocEvent, func()) {
	wait, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	for {
		f.mu.Lock()
		if f.connected {
			ch := make(chan ocEvent, 256)
			if f.bySession[sid] == nil {
				f.bySession[sid] = map[chan ocEvent]struct{}{}
			}
			f.bySession[sid][ch] = struct{}{}
			f.mu.Unlock()
			return ch, func() {
				f.mu.Lock()
				delete(f.bySession[sid], ch)
				if len(f.bySession[sid]) == 0 {
					delete(f.bySession, sid)
				}
				f.mu.Unlock()
			}
		}
		ready := f.ready
		f.mu.Unlock()
		select {
		case <-ready:
		case <-wait.Done():
			return nil, func() {}
		}
	}
}

func (f *eventFeed) dispatch(raw []byte) {
	var event ocEvent
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	switch event.Type {
	case "session.text.started", "session.text.delta", "session.text.ended", "session.step.started",
		"message.part.delta", "message.part.updated", "session.idle", "session.status", "session.error", "session.interrupted",
		"session.execution.succeeded", "session.execution.failed", "session.execution.interrupted":
	default:
		return
	}
	if event.Data.SessionID == "" && event.Data.Part.SessionID == "" {
		event.Data = event.Properties
	}
	id := event.Data.SessionID
	if id == "" {
		id = event.Data.Part.SessionID
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for ch := range f.bySession[id] {
		select {
		case ch <- event:
		default:
			// A slow turn must not block the shared connection or silently miss events.
			delete(f.bySession[id], ch)
			close(ch)
		}
	}
}

var eventPath = "/api/event?" + url.Values{"types": {
	"session.text.started", "session.text.delta", "session.text.ended", "session.step.started",
	"session.execution.succeeded", "session.execution.failed", "session.execution.interrupted",
	"session.idle", "session.status",
}}.Encode()

func (f *eventFeed) read(ctx context.Context, api upstream) bool {
	res, err := api.request(ctx, http.MethodGet, eventPath, "", "text/event-stream", 0, nil)
	var status *upstreamStatus
	if errors.As(err, &status) && status.code == 400 {
		res, err = api.request(ctx, http.MethodGet, "/api/event", "", "text/event-stream", 0, nil)
	}
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return false
	}
	f.setConnected(true)
	defer f.setConnected(false)
	s := bufio.NewScanner(res.Body)
	const limit = 4 << 20
	s.Buffer(make([]byte, 16<<10), limit)
	var data []byte
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			if len(data) > 0 {
				f.dispatch(data)
				data = data[:0]
			}
		} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			value, _ = bytes.CutPrefix(value, []byte(" "))
			if len(data)+len(value) >= limit {
				return true
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	return true
}

func (f *eventFeed) run(ctx context.Context, api upstream) {
	const minBackoff = 100 * time.Millisecond
	backoff := minBackoff
	for ctx.Err() == nil {
		since := time.Now()
		if f.read(ctx, api) && time.Since(since) > 5*time.Second {
			backoff = minBackoff
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			backoff = min(5*time.Second, backoff*2)
		}
	}
}

type ocModel struct {
	Provider string `json:"providerID"`
	ID       string `json:"id"`
	Variant  string `json:"variant,omitempty"`
}

type ocPermission struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

type ocSettings struct {
	Agent       string         `json:"agent"`
	Model       ocModel        `json:"model"`
	Permissions []ocPermission `json:"permissions"`
}

type openCode struct {
	upstream
	feed     eventFeed
	settings ocSettings
	project  string
	store    *store[string]
	turns    [64]chan struct{}
}

func newOpenCode(ctx context.Context, u upstream, path, project string, settings ocSettings) *openCode {
	o := &openCode{upstream: u, project: project, settings: settings,
		store: loadStore(path, func(_ string, sid string) bool { return sessionIDRE.MatchString(sid) }),
		feed:  eventFeed{ready: make(chan struct{}), bySession: make(map[string]map[chan ocEvent]struct{})}}
	for i := range o.turns {
		o.turns[i] = make(chan struct{}, 1)
	}
	go o.feed.run(ctx, u)
	return o
}

func (o *openCode) get(device string) (string, error) {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	if o.store.data == nil {
		return "", o.store.invalid()
	}
	return o.store.data[device], nil
}

func (o *openCode) save(device, sid string) error {
	o.store.mu.Lock()
	defer o.store.mu.Unlock()
	return o.store.saveLocked(device, sid, sid == "")
}

func (o *openCode) lock(ctx context.Context, device string) (func(), error) {
	h := fnv.New32a()
	_, _ = h.Write([]byte(device))
	gate := o.turns[h.Sum32()%uint32(len(o.turns))]
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, o.unavailable(ctx.Err())
	}
}

func (o *openCode) reset(ctx context.Context, device string) error {
	unlock, err := o.lock(ctx, device)
	if err != nil {
		return err
	}
	defer unlock()
	return o.save(device, "")
}

func ocData[T any](ctx context.Context, api upstream, method, path string, in any) (T, error) {
	var envelope struct{ Data *T }
	var zero T
	if err := api.jsonCall(ctx, method, path, in, &envelope); err != nil {
		return zero, err
	}
	if envelope.Data == nil {
		return zero, api.invalid()
	}
	return *envelope.Data, nil
}

func (o *openCode) ensureSession(ctx context.Context, device string) (string, error) {
	sid, err := o.get(device)
	if err != nil {
		return "", err
	}
	type session struct {
		ID string `json:"id"`
		ocSettings
	}
	settings := o.settings
	if strings.HasPrefix(device, "scope-virt-") {
		// Public chat must not grant a general HTTP or Code Mode client.
		settings.Permissions = append(slices.Clone(settings.Permissions),
			ocPermission{"execute", "*", "deny"}, ocPermission{"webfetch", "*", "deny"},
			ocPermission{"echoear_*", "*", "deny"})
	}
	if sid != "" {
		path := "/api/session/" + sid
		current, err := ocData[session](ctx, o.upstream, http.MethodGet, path, nil)
		if err == nil {
			if current.ID != sid {
				return "", o.invalid()
			}
			s := settings
			if s.Agent != current.Agent {
				if err := o.jsonCall(ctx, http.MethodPost, path+"/agent", object{"agent": s.Agent}, nil); err != nil {
					return "", err
				}
			}
			if s.Model != current.Model {
				if err := o.jsonCall(ctx, http.MethodPost, path+"/model", object{"model": s.Model}, nil); err != nil {
					return "", err
				}
			}
			if !slices.Equal(s.Permissions, current.Permissions) {
				if err := o.jsonCall(ctx, http.MethodPatch, path, object{"permissions": s.Permissions}, nil); err != nil {
					return "", err
				}
			}
			return sid, nil
		}
		var status *upstreamStatus
		if !errors.As(err, &status) || status.code != 404 {
			return "", err
		}
	}
	in := struct {
		Title    string `json:"title"`
		Location object `json:"location"`
		ocSettings
	}{"EchoEar: " + device, object{"directory": o.project}, settings}
	created, err := ocData[session](ctx, o.upstream, http.MethodPost, "/api/session", in)
	if err != nil {
		return "", err
	}
	if !sessionIDRE.MatchString(created.ID) {
		return "", o.invalid()
	}
	if err := o.save(device, created.ID); err != nil {
		return "", err
	}
	return created.ID, nil
}

type ocMessage struct {
	ID, Type, Finish, Outcome string
	Text                      string
	Time                      struct{ Created int64 }
	Metadata                  struct{ EchoEarSource string }
	Content                   []*ocMessagePart
	Parts                     []*ocMessagePart
	Error                     *struct{ Status int }
}

type ocMessagePart struct {
	Type, Name, Tool string
	Text             *string
	State            struct{ Input struct{ Title string } }
}

func (m ocMessage) chat(source string) chatEntry {
	entry := chatEntry{ID: m.ID, Role: m.Type, Source: source, Timestamp: time.UnixMilli(m.Time.Created).UTC()}
	switch m.Type {
	case "user":
		entry.Sender, entry.Text = "You", m.Text
	case "assistant":
		entry.Sender = "EchoEar"
	case "system":
		entry.Sender, entry.Text = "System", m.Text
	default:
		return chatEntry{}
	}
	parts := m.Content
	if parts == nil {
		parts = m.Parts
	}
	var text, notes []string
	for _, p := range parts {
		if p == nil {
			continue
		}
		if p.Type == "text" && p.Text != nil {
			text = append(text, *p.Text)
		}
		name := p.Name
		if name == "" {
			name = p.Tool
		}
		if action, ok := strings.CutPrefix(name, "echoear_"); p.Type == "tool" && ok {
			label := map[string]string{"devices": "Listed cats", "snapshot": "Checked the screen", "scene": "Showed a scene",
				"patch": "Updated the screen", "speak": "Spoke aloud", "chime": "Played a chime", "intercom": "Sent an intercom message",
				"wait_event": "Waited for an interaction"}[action]
			if label != "" && !slices.Contains(notes, label) {
				notes = append(notes, label)
			}
			if action == "scene" {
				entry.SceneTitle = shortText(p.State.Input.Title, 48, false)
			}
		}
	}
	if entry.Text == "" {
		entry.Text = strings.Join(text, "\n")
	}
	if m.Type == "user" && strings.HasPrefix(entry.Text, "[EchoEar: ") {
		if first, rest, ok := strings.Cut(entry.Text, "\n"); ok && strings.HasSuffix(strings.TrimSuffix(first, "\r"), "]") {
			entry.Text = rest
		}
	}
	if m.Type == "assistant" {
		entry.Text = fenced.ReplaceAllString(entry.Text, "")
	}
	entry.Text = strings.TrimSpace(entry.Text)
	entry.ToolSummary = strings.Join(notes, "; ")
	if entry.Text == "" && entry.ToolSummary != "" {
		entry.Source = "mcp"
	}
	return entry
}

func (o *openCode) GetSessionMessages(ctx context.Context, sid string) ([]chatEntry, error) {
	if !sessionIDRE.MatchString(sid) {
		return nil, problem(400, "Invalid session ID")
	}
	type page struct {
		Data   []ocMessage
		Cursor struct{ Next string }
	}
	params := url.Values{"order": {"asc"}, "limit": {"50"}}
	entries := []chatEntry{}
	source := "text"
	seen := make(map[string]bool)
	for count := 0; count < chatLimit; {
		var result page
		if err := o.jsonCall(ctx, http.MethodGet, "/api/session/"+sid+"/message?"+params.Encode(), nil, &result); err != nil {
			return nil, err
		}
		for _, message := range result.Data[:min(len(result.Data), chatLimit-count)] {
			count++
			if message.Type == "user" {
				source = "text"
				if message.Metadata.EchoEarSource == "voice" {
					source = "voice"
				}
			}
			if entry := message.chat(source); entry.ID != "" && (entry.Text != "" || entry.ToolSummary != "") {
				entries = append(entries, entry)
			}
		}
		if result.Cursor.Next == "" || seen[result.Cursor.Next] || len(result.Data) == 0 {
			break
		}
		seen[result.Cursor.Next] = true
		params.Del("order")
		params.Set("cursor", result.Cursor.Next)
	}
	return entries, nil
}

var errNoFinal = problem(502, "OpenCode did not return a final assistant reply")

func finalReply(messages []ocMessage, promptID string) (string, ocMessage, error) {
	var final ocMessage
	reply := ""
	for _, m := range messages {
		if m.ID == "" || m.Type == "" {
			return "", final, problem(502, "OpenCode returned invalid data")
		}
		if m.ID == promptID {
			if final.ID != "" {
				return reply, final, nil
			}
			break
		}
		if m.Type == "idle" && m.Outcome != "succeeded" {
			if m.Error != nil && m.Error.Status == 429 {
				return "", final, problem(503, "OpenCode model usage limit reached")
			}
			return "", final, problem(502, "OpenCode turn did not complete successfully")
		}
		if m.Type == "assistant" && final.ID == "" && (m.Finish == "stop" || m.Finish == "length") {
			final = m
			if m.Content == nil {
				return "", final, problem(502, "OpenCode returned invalid data")
			}
			var parts []string
			for _, part := range m.Content {
				if part == nil || part.Type == "text" && part.Text == nil {
					return "", final, problem(502, "OpenCode returned invalid data")
				}
				if part.Type == "text" {
					parts = append(parts, *part.Text)
				}
			}
			reply = strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	return "", final, errNoFinal
}

type streamedText struct {
	sentenceExtractor
	messageID string
	parts     map[string]string
	byOrdinal map[int]string
	ordinal   int
	havePart  bool
}

func (t *streamedText) addEvent(event ocEvent) []string {
	d := event.Data
	id := d.AssistantMessageID
	if id == "" {
		id = d.MessageID
	}
	if id == "" {
		id = d.Part.MessageID
	}
	if id != "" && id != t.messageID {
		t.messageID = id
		t.sentenceExtractor = sentenceExtractor{}
		clear(t.parts)
		clear(t.byOrdinal)
		t.havePart = false
	}
	switch event.Type {
	case "session.text.started":
		if t.havePart && d.Ordinal != t.ordinal {
			t.ordinal = d.Ordinal
			return t.add("\n")
		}
		t.ordinal, t.havePart = d.Ordinal, true
	case "session.text.delta":
		if t.halted || t.havePart && d.Ordinal != t.ordinal {
			t.halted = true
			return nil
		}
		if t.byOrdinal == nil {
			t.byOrdinal = map[int]string{}
		}
		t.ordinal, t.havePart = d.Ordinal, true
		t.byOrdinal[d.Ordinal] += d.Delta
		return t.add(d.Delta)
	case "session.text.ended":
		if tail, ok := strings.CutPrefix(d.Text, t.byOrdinal[d.Ordinal]); ok && (!t.havePart || d.Ordinal == t.ordinal) {
			return t.add(tail + "\n")
		}
		t.halted = true // The authoritative final message may differ from the deltas.
	case "message.part.updated":
		if d.Part.Type == "text" {
			if t.parts == nil {
				t.parts = map[string]string{}
			}
			prev := t.parts[d.Part.ID]
			if tail, ok := strings.CutPrefix(d.Part.Text, prev); ok {
				t.parts[d.Part.ID] = d.Part.Text
				return t.add(tail)
			}
		}
	case "message.part.delta":
		if prev, ok := t.parts[d.PartID]; ok && d.Field == "text" {
			t.parts[d.PartID] = prev + d.Delta
			return t.add(d.Delta)
		}
	}
	return nil
}

func (o *openCode) RunTurnStreaming(ctx context.Context, sid, prompt string, onSentence func(string)) (string, error) {
	return o.runTurnStreaming(ctx, sid, prompt, onSentence, "", nil)
}

func (o *openCode) runTurnStreaming(ctx context.Context, sid, prompt string, onSentence func(string), source string, onChat func(chatEntry)) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	events, unsubscribe := o.feed.subscribe(ctx, sid)
	defer unsubscribe()
	path := "/api/session/" + sid
	input := object{"text": prompt}
	if source != "" {
		input["metadata"] = object{"echoEarSource": source}
	}
	result, err := ocData[struct{ ID string }](ctx, o.upstream, http.MethodPost, path+"/prompt", input)
	if err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", o.invalid()
	}
	if onChat != nil {
		message := ocMessage{ID: result.ID, Type: "user", Text: prompt}
		message.Time.Created = time.Now().UnixMilli()
		onChat(message.chat(source))
	}
	readFinal := func() (string, error) {
		msgs, err := ocData[[]ocMessage](ctx, o.upstream, http.MethodGet, path+"/message?order=desc&limit=32", nil)
		if err != nil {
			return "", err
		}
		reply, final, err := finalReply(msgs, result.ID)
		if err == nil && onChat != nil {
			if entry := final.chat(source); entry.Text != "" || entry.ToolSummary != "" {
				onChat(entry)
			}
		}
		return reply, err
	}
	var streamed streamedText
stream:
	for events != nil {
		select {
		case <-ctx.Done():
			return "", o.unavailable(ctx.Err())
		case event, ok := <-events:
			if !ok {
				break stream
			}
			switch event.Type {
			case "session.status":
				if event.Data.Status.Type != "idle" {
					continue
				}
				fallthrough
			case "session.idle", "session.execution.succeeded", "session.execution.failed", "session.execution.interrupted", "session.error", "session.interrupted":
				reply, err := readFinal()
				if !errors.Is(err, errNoFinal) {
					return reply, err
				}
				break stream
			default:
				if onSentence != nil {
					for _, s := range streamed.addEvent(event) {
						onSentence(s)
					}
				}
			}
		}
	}
	if err := o.jsonCall(ctx, http.MethodPost, "/api/experimental/session/"+sid+"/wait", nil, nil); err != nil {
		return "", err
	}
	return readFinal()
}

func (o *openCode) ask(ctx context.Context, device, prompt string, onSentence func(string), source string, begin func() func(), onChat func(chatEntry)) (reply, sid string, err error) {
	unlock, err := o.lock(ctx, device)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	sid, err = o.ensureSession(ctx, device)
	if err != nil {
		return "", "", err
	}
	if begin != nil {
		finish := begin()
		defer finish()
	}
	reply, err = o.runTurnStreaming(ctx, sid, prompt, onSentence, source, onChat)
	return reply, sid, err
}
