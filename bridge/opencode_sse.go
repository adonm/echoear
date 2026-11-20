package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

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
		"session.tool.input.started", "session.tool.called",
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
	"session.tool.input.started", "session.tool.called",
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
	onFirst   func()
	messageID string
	parts     map[string]string
	byOrdinal map[int]*strings.Builder
	ordinal   int
	havePart  bool
}

func (t *streamedText) add(delta string) []string {
	out := t.sentenceExtractor.add(delta)
	if t.onFirst != nil && t.hasLetter {
		t.onFirst()
		t.onFirst = nil
	}
	return out
}

func (t *streamedText) addEvent(event ocEvent) []string {
	d := event.Data
	switch event.Type {
	case "session.text.started", "session.text.delta", "session.text.ended":
	case "message.part.updated":
		if d.Part.Type != "text" {
			return nil
		}
	case "message.part.delta":
		if d.Field != "text" {
			return nil
		}
	default:
		return nil
	}
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
			t.byOrdinal = map[int]*strings.Builder{}
		}
		t.ordinal, t.havePart = d.Ordinal, true
		part := t.byOrdinal[d.Ordinal]
		if part == nil {
			part = new(strings.Builder)
			t.byOrdinal[d.Ordinal] = part
		}
		part.WriteString(d.Delta)
		return t.add(d.Delta)
	case "session.text.ended":
		var prior string
		if part := t.byOrdinal[d.Ordinal]; part != nil {
			prior = part.String()
		}
		if tail, ok := strings.CutPrefix(d.Text, prior); ok && (!t.havePart || d.Ordinal == t.ordinal) {
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
	return o.runTurnStreaming(ctx, sid, prompt, turnStream{sentence: onSentence}, "", nil)
}

type turnStream struct {
	sentence func(string)
	tool     func()
}

func (o *openCode) runTurnStreaming(ctx context.Context, sid, prompt string, stream turnStream, source string, onChat func(chatEntry)) (string, error) {
	start := time.Now()
	trace := traceFrom(ctx)
	defer func() { trace.measure(diagLLMTotal, time.Since(start)) }()
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	events, unsubscribe := o.feed.subscribe(ctx, sid)
	defer unsubscribe()
	path := "/api/session/" + sid
	input := object{"text": prompt}
	if source != "" {
		input["metadata"] = object{"echoEarSource": source}
	}
	if ctx.Err() != nil {
		return "", o.unavailable(ctx.Err())
	}
	// Finish admitting an in-flight prompt before interrupting, even if the
	// local turn has been cancelled in the meantime.
	submit, stopSubmit := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	result, err := ocData[struct{ ID string }](submit, o.upstream, http.MethodPost, path+"/prompt", input)
	stopSubmit()
	if ctx.Err() != nil {
		return "", o.unavailable(ctx.Err())
	}
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
	usedTool := false
	markTool := func() {
		if !usedTool && stream.tool != nil {
			stream.tool()
		}
		usedTool = true
	}
	readFinal := func() (string, error) {
		params := url.Values{"order": {"desc"}, "limit": {"32"}}
		var msgs []ocMessage
		seen := make(map[string]bool)
		for range 8 {
			var page ocMessagePage
			if err := o.jsonCall(ctx, http.MethodGet, path+"/message?"+params.Encode(), nil, &page); err != nil {
				return "", err
			}
			if page.Data == nil {
				return "", o.invalid()
			}
			msgs = append(msgs, page.Data...)
			next := page.Cursor.Next
			if len(page.Data) == 0 || next == "" || seen[next] ||
				slices.ContainsFunc(page.Data, func(m ocMessage) bool { return m.ID == result.ID }) {
				break
			}
			seen[next] = true
			params.Del("order")
			params.Set("cursor", next)
		}
		reply, final, err := finalReply(msgs, result.ID)
		if err == nil && !usedTool && stream.tool != nil {
			// A disconnected feed can miss tool events. Check only this turn.
			for _, msg := range msgs {
				if msg.ID == result.ID {
					break
				}
				if msg.Type == "tool" || msg.Finish == "tool-calls" {
					markTool()
					break
				}
				for _, parts := range [][]*ocMessagePart{msg.Content, msg.Parts} {
					for _, part := range parts {
						if part != nil && (part.Type == "tool" || part.Type == "tool-call") {
							markTool()
						}
					}
				}
			}
		}
		if err == nil && onChat != nil {
			if entry := final.chat(source); entry.Text != "" || entry.ToolSummary != "" {
				onChat(entry)
			}
		}
		return reply, err
	}
	var streamed streamedText
	if trace != nil {
		streamed.onFirst = func() { trace.measure(diagLLMFirst, time.Since(start)) }
	}
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
				if strings.HasPrefix(event.Type, "session.tool.") ||
					event.Type == "message.part.updated" && event.Data.Part.Type == "tool" {
					markTool()
				}
				if !usedTool && stream.sentence != nil || streamed.onFirst != nil {
					for _, s := range streamed.addEvent(event) {
						if !usedTool && stream.sentence != nil {
							stream.sentence(s)
						}
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
