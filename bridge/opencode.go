package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const turnTimeout = 90 * time.Second

type fixedEndpoint endpoint

func (e fixedEndpoint) get(bool) (endpoint, error) { return endpoint(e), nil }

func openCodeUpstream(client *http.Client, state, base, password string) (upstream, error) {
	if base == "" {
		return upstream{"OpenCode", client, &fileCache[endpoint]{
			path: filepath.Join(state, "opencode/service.json"), read: openCodeEndpoint,
		}}, nil
	}
	dst, err := localOpenCodeEndpoint(base, password)
	return upstream{"OpenCode", client, fixedEndpoint(dst)}, err
}

func openCodeEndpoint(raw []byte) (endpoint, error) {
	var info struct{ URL, Password string }
	if json.Unmarshal(raw, &info) != nil || info.Password == "" {
		return endpoint{}, errors.New("invalid OpenCode service configuration")
	}
	return localOpenCodeEndpoint(info.URL, info.Password)
}

func localOpenCodeEndpoint(base, password string) (endpoint, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" || u.User != nil || u.ForceQuery || u.RawQuery != "" ||
		u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return endpoint{}, errors.New("invalid OpenCode service URL")
	}
	host := "127.0.0.1"
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0":
	case "::", "::1":
		host = "::1"
	default:
		return endpoint{}, errors.New("not a local OpenCode service URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return endpoint{}, errors.New("invalid OpenCode service port")
	}
	dst := endpoint{base: "http://" + net.JoinHostPort(host, u.Port())}
	if password != "" {
		dst.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+password))
	}
	return dst, nil
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

	mu         sync.Mutex
	active     map[string]context.CancelFunc
	interrupts map[string]bool
	resets     map[string]*ocReset
}

type ocReset struct {
	count int
	done  chan struct{}
}

func newOpenCode(ctx context.Context, u upstream, db *bridgeDB, project string, settings ocSettings) *openCode {
	o := &openCode{upstream: u, project: project, settings: settings,
		store:  sessionStore(db),
		active: make(map[string]context.CancelFunc), interrupts: make(map[string]bool), resets: make(map[string]*ocReset),
		feed: eventFeed{ready: make(chan struct{}), bySession: make(map[string]map[chan ocEvent]struct{})}}
	for i := range o.turns {
		o.turns[i] = make(chan struct{}, 1)
	}
	go o.feed.run(ctx, u)
	return o
}

func (o *openCode) get(device string) (string, error) {
	sid, _, err := o.store.get(device)
	return sid, err
}

func (o *openCode) save(device, sid string) error {
	return o.store.save(device, sid, sid == "")
}

func (o *openCode) lock(ctx context.Context, device string) (func(), error) {
	return o.lockGate(ctx, o.turnGate(device))
}

func (o *openCode) turnGate(device string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(device))
	return int(h.Sum32() % uint32(len(o.turns)))
}

func (o *openCode) lockGate(ctx context.Context, index int) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, o.unavailable(err)
	}
	gate := o.turns[index]
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, o.unavailable(ctx.Err())
	}
}

func (o *openCode) interrupt(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := o.jsonCall(ctx, http.MethodPost, "/api/session/"+sid+"/interrupt", nil, nil)
	var status *upstreamStatus
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return nil
	}
	return err // The API reports an idle no-op as success.
}

func (o *openCode) requestInterrupt(device string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.resets[device] != nil {
		return false // The mapping change already owns the interrupt for the old session.
	}
	return o.markInterrupted(device)
}

func (o *openCode) markInterrupted(device string) bool {
	if cancel := o.active[device]; cancel != nil {
		cancel()
	}
	queued := o.interrupts[device]
	o.interrupts[device] = true
	return !queued
}

// Hold the turn gate through the interrupt. A failed interrupt stays pending
// so neither a replacement prompt nor a reset can race the old execution.
func (o *openCode) flushInterrupt(ctx context.Context, device string) error {
	o.mu.Lock()
	pending := o.interrupts[device]
	o.mu.Unlock()
	if !pending {
		return nil
	}
	sid, err := o.get(device)
	if err == nil {
		err = o.interrupt(ctx, sid)
	}
	if err == nil {
		o.mu.Lock()
		delete(o.interrupts, device)
		o.mu.Unlock()
	}
	return err
}

func (o *openCode) interruptDevice(device string) {
	if !o.requestInterrupt(device) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
		defer cancel()
		if unlock, err := o.lock(ctx, device); err == nil {
			defer unlock()
			_ = o.flushInterrupt(ctx, device)
		}
	}()
}

func (o *openCode) reset(ctx context.Context, device string) error {
	return o.prepareReset(device)(ctx)
}

func (o *openCode) prepareReset(device string) func(context.Context) error {
	run := o.prepareMappingChange(device)
	return func(ctx context.Context) error {
		return run(ctx, func() error { return o.save(device, "") })
	}
}

// Block new prompts and interrupt the old mappings before changing them.
func (o *openCode) prepareMappingChange(devices ...string) func(context.Context, func() error) error {
	gates := make([]int, 0, len(devices))
	o.mu.Lock()
	for _, device := range devices {
		reset := o.resets[device]
		if reset == nil {
			reset = &ocReset{done: make(chan struct{})}
			o.resets[device] = reset
		}
		reset.count++
		o.markInterrupted(device)
		gates = append(gates, o.turnGate(device))
	}
	o.mu.Unlock()
	slices.Sort(gates)
	gates = slices.Compact(gates) // Two devices may hash to the same gate.
	return func(ctx context.Context, apply func() error) error {
		defer func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			for _, device := range devices {
				reset := o.resets[device]
				reset.count--
				if reset.count == 0 {
					delete(o.resets, device)
					close(reset.done)
				}
			}
		}()
		for _, gate := range gates {
			unlock, err := o.lockGate(ctx, gate)
			if err != nil {
				return err
			}
			defer unlock()
		}
		for _, device := range devices {
			if err := o.flushInterrupt(ctx, device); err != nil {
				return err
			}
		}
		return apply()
	}
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

func (o *openCode) ReconnectMCP(ctx context.Context, name string) error {
	path := "/api/experimental/mcp/" + url.PathEscape(name) + "/connect"
	if o.project != "" {
		path += "?" + url.Values{"location[directory]": {o.project}}.Encode()
	}
	return o.jsonCall(ctx, http.MethodPost, path, nil, nil)
}

// CompactSession returns false without an error for an ineligible or stale session.
func (o *openCode) CompactSession(ctx context.Context, device, sid string) (bool, error) {
	if sid == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, turnTimeout)
	defer cancel()
	unlock, err := o.lock(ctx, device)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, _, err := o.store.getContext(ctx, device)
	if err != nil || current != sid {
		return false, err
	}
	if !sessionIDRE.MatchString(sid) {
		return false, problem(502, "Invalid stored OpenCode session ID")
	}
	path := "/api/session/" + sid
	isMissing := func(err error) bool {
		var status *upstreamStatus
		return errors.As(err, &status) && status.code == http.StatusNotFound
	}
	session, err := ocData[struct{ ID string }](ctx, o.upstream, http.MethodGet, path, nil)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if session.ID != sid {
		return false, o.invalid()
	}
	item, err := ocData[struct{ ID, SessionID, Type string }](ctx, o.upstream, http.MethodPost, path+"/compact", object{"delivery": "queue"})
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if item.ID == "" || item.SessionID != sid || item.Type != "compaction" {
		return false, o.invalid()
	}
	// Admission is not completion. Keep the gate until the result is projected.
	err = o.jsonCall(ctx, http.MethodPost, "/api/experimental/session/"+sid+"/wait", nil, nil)
	if isMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	message, err := ocData[struct {
		ID, Type, Status string
		Error            struct{ Type, Message string }
	}](ctx, o.upstream, http.MethodGet, path+"/message/"+url.PathEscape(item.ID), nil)
	if err != nil {
		return false, err
	}
	if message.ID != item.ID || message.Type != "compaction" {
		return false, o.invalid()
	}
	// OpenCode reports this when the session has no history beyond its last compaction.
	if message.Status == "failed" && message.Error.Type == "compaction.unavailable" && message.Error.Message == "Nothing to compact yet" {
		return false, nil
	}
	if message.Status != "completed" {
		return false, problem(502, "OpenCode did not complete session compaction")
	}
	return true, nil
}

func echoEarTitle(device, character string) string {
	if character = strings.TrimSpace(character); character != "" && character != device {
		return "EchoEar: " + character + " (" + device + ")"
	}
	return "EchoEar: " + device
}

func (o *openCode) ensureSession(ctx context.Context, device, character string) (string, error) {
	sid, err := o.get(device)
	if err != nil {
		return "", err
	}
	type session struct {
		ID string `json:"id"`
		ocSettings
	}
	settings := o.settings
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
	}{echoEarTitle(device, character), object{"directory": o.project}, settings}
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

func sessionMatchesDevice(title, deviceID string) bool {
	name, ok := strings.CutPrefix(title, "EchoEar: ")
	if !ok || deviceID == "" {
		return false
	}
	if strings.EqualFold(name, deviceID) {
		return true
	}
	if id, ok := strings.CutPrefix(name, "scope-"); ok && strings.EqualFold(id, deviceID) {
		return true
	}
	if name, ok := strings.CutSuffix(name, ")"); ok {
		if i := strings.LastIndex(name, " ("); i >= 0 {
			return strings.EqualFold(name[i+2:], deviceID)
		}
	}
	return false
}

func (o *openCode) ListDeviceSessions(ctx context.Context, deviceID, activeSID string) ([]string, error) {
	return o.listDeviceSessions(ctx, deviceID, activeSID, true)
}

func (o *openCode) listDeviceSessions(ctx context.Context, deviceID, activeSID string, fallback bool) ([]string, error) {
	type session struct {
		ID, Title string
		Time      struct{ Created int64 }
	}
	type page struct {
		Data   []session
		Cursor struct{ Next string }
	}
	var matches []session
	seenIDs, seenCursors := make(map[string]bool), make(map[string]bool)
	params := url.Values{"limit": {"200"}}
	for {
		var result page
		if err := o.jsonCall(ctx, http.MethodGet, "/api/session?"+params.Encode(), nil, &result); err != nil {
			if fallback && activeSID != "" {
				return []string{activeSID}, nil
			}
			return nil, err
		}
		for _, s := range result.Data {
			if sessionIDRE.MatchString(s.ID) && !seenIDs[s.ID] && (s.ID == activeSID || sessionMatchesDevice(s.Title, deviceID)) {
				matches = append(matches, s)
				seenIDs[s.ID] = true
			}
		}
		next := result.Cursor.Next
		if next == "" || seenCursors[next] {
			break
		}
		seenCursors[next] = true
		params.Set("cursor", next)
	}
	slices.SortFunc(matches, func(a, b session) int {
		return cmp.Or(cmp.Compare(a.Time.Created, b.Time.Created), strings.Compare(a.ID, b.ID))
	})
	ids := make([]string, 0, len(matches))
	for _, s := range matches {
		ids = append(ids, s.ID)
	}
	if activeSID != "" && !seenIDs[activeSID] {
		ids = append(ids, activeSID)
	}
	return ids, nil
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
			label := map[string]string{"state": "Checked live state", "patch": "Changed saved or device state",
				"app": "Used an app", "emit": "Sent a signal"}[action]
			if label != "" && !slices.Contains(notes, label) {
				notes = append(notes, label)
			}
			if action == "app" {
				entry.SceneTitle = shortText(p.State.Input.Title, 48, false)
			}
		}
	}
	if entry.Text == "" {
		entry.Text = strings.Join(text, "\n")
	}
	if m.Type == "user" {
		entry.Text = stripEchoEarHeader(entry.Text)
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

type ocMessagePage struct {
	Data   []ocMessage
	Cursor struct{ Next string }
}

func (o *openCode) GetSessionMessages(ctx context.Context, sid string) ([]chatEntry, error) {
	if !sessionIDRE.MatchString(sid) {
		return nil, problem(400, "Invalid session ID")
	}
	params := url.Values{"order": {"asc"}, "limit": {"50"}}
	entries := []chatEntry{}
	source := "text"
	seen := make(map[string]bool)
	for {
		var result ocMessagePage
		if err := o.jsonCall(ctx, http.MethodGet, "/api/session/"+sid+"/message?"+params.Encode(), nil, &result); err != nil {
			return nil, err
		}
		for _, message := range result.Data {
			if message.Type == "assistant" && message.Finish != "" && message.Finish != "stop" && message.Finish != "length" {
				continue
			}
			if message.Type == "user" {
				switch message.Metadata.EchoEarSource {
				case "typed", "voice", "tap":
					source = message.Metadata.EchoEarSource
				default:
					source = "text"
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

func (o *openCode) lockPrompt(ctx context.Context, device string) (context.Context, func(), error) {
	for {
		unlock, err := o.lock(ctx, device)
		if err != nil {
			return nil, nil, err
		}
		if err := o.flushInterrupt(ctx, device); err != nil {
			unlock()
			return nil, nil, err
		}
		o.mu.Lock()
		if reset := o.resets[device]; reset != nil {
			o.mu.Unlock()
			unlock()
			select {
			case <-reset.done:
				continue
			case <-ctx.Done():
				return nil, nil, o.unavailable(ctx.Err())
			}
		}
		if !o.interrupts[device] {
			turn, cancel := context.WithCancel(ctx)
			o.active[device] = cancel
			o.mu.Unlock()
			return turn, func() {
				defer unlock()
				o.mu.Lock()
				delete(o.active, device)
				o.mu.Unlock()
				cancel()
				_ = o.flushInterrupt(context.WithoutCancel(turn), device)
			}, nil
		}
		o.mu.Unlock()
		unlock()
	}
}

func (o *openCode) ask(ctx context.Context, who peer, prompt string, onSentence func(string), source string, begin func() func(), onChat func(chatEntry)) (reply, sid string, err error) {
	return o.askPrepared(ctx, who.DeviceID, func() (string, string) { return who.Character, prompt }, turnStream{sentence: onSentence}, source, begin, onChat)
}

func (o *openCode) askPrepared(ctx context.Context, device string, prepare func() (string, string), stream turnStream, source string, begin func() func(), onChat func(chatEntry)) (reply, sid string, err error) {
	waiting := time.Now()
	trace := traceFrom(ctx)
	ctx, unlock, err := o.lockPrompt(ctx, device)
	if err != nil {
		return "", "", err
	}
	trace.measure(diagGate, time.Since(waiting))
	defer unlock()
	character, prompt := prepare()
	sid, err = o.ensureSession(ctx, device, character)
	if err != nil {
		return "", "", err
	}
	if begin != nil {
		finish := begin()
		defer finish()
	}
	reply, err = o.runTurnStreaming(ctx, sid, prompt, stream, source, onChat)
	if err != nil {
		o.requestInterrupt(device)
	}
	return reply, sid, err
}
