package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	mcpProtocol = "2025-11-25"
	mcpLimit    = 128 << 10
	maxNodes    = 48
	exitNode    = "echoear-exit"
	eventLimit  = 64
)

type rpcRequest struct {
	Version string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcError(id json.RawMessage, code int, message string) object {
	return object{"jsonrpc": "2.0", "id": id, "error": object{"code": code, "message": message}}
}

func validRPCID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	switch id[0] {
	case '"', '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return true
	}
	return false
}

func (b *bridge) serveMCP(w http.ResponseWriter, r *http.Request) {
	if b.token == "" || !tokenMatches(r.Header.Values("X-EchoEar-Token"), b.token) &&
		!tokenMatches(r.Header.Values("Authorization"), "Bearer "+b.token) {
		sendError(w, problem(401, "Unauthorized"))
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			sendError(w, problem(403, "Unexpected MCP origin"))
			return
		}
	}
	switch r.Method {
	case http.MethodDelete:
		w.WriteHeader(204) // Stateless transport.
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "POST, DELETE")
		sendError(w, problem(405, "Method not allowed"))
		return
	}
	if version := r.Header.Get("MCP-Protocol-Version"); version != "" && !supportedMCP(version) {
		sendError(w, problem(400, "Unsupported MCP protocol version"))
		return
	}
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != jsonType {
		sendError(w, problem(415, "Expected application/json"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, mcpLimit))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			sendError(w, problem(413, "MCP request too large"))
		} else {
			sendError(w, problem(400, "Cannot read MCP request"))
		}
		return
	}
	if !json.Valid(raw) {
		sendJSON(w, 400, rpcError(nil, -32700, "Parse error"))
		return
	}
	var req rpcRequest
	if json.Unmarshal(raw, &req) != nil || req.Version != "2.0" || req.Method == "" || !validRPCID(req.ID) {
		sendJSON(w, 400, rpcError(nil, -32600, "Invalid request"))
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	params, err := mcpObject(req.Params)
	if err != nil {
		sendJSON(w, 200, rpcError(req.ID, -32602, "Invalid params"))
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		version := str(params["protocolVersion"])
		if !supportedMCP(version) {
			version = mcpProtocol
		}
		result = object{"protocolVersion": version, "capabilities": object{"tools": object{}},
			"serverInfo":   object{"name": "echoear", "version": "0.3.0"},
			"instructions": "Use the exact EchoEar device_id from the conversation. For interactive scenes, pass the latest event_cursor to wait_event."}
	case "ping":
		result = object{}
	case "tools/list":
		result = object{"tools": mcpTools}
	case "tools/call":
		name := str(params["name"])
		args, ok := params["arguments"].(object)
		if params["arguments"] == nil {
			args, ok = object{}, true
		}
		if name == "" || !ok || !slices.ContainsFunc(mcpTools, func(tool object) bool { return tool["name"] == name }) {
			sendJSON(w, 200, rpcError(req.ID, -32602, "Unknown tool or invalid arguments"))
			return
		}
		out, err := b.callMCP(r.Context(), name, args)
		if err != nil {
			out = object{"error": err.Error()}
			var conflict *sceneConflict
			if errors.As(err, &conflict) {
				out["rev"] = conflict.rev
			}
		}
		wire, _ := json.Marshal(out)
		result = object{"content": []object{{"type": "text", "text": string(wire)}}, "structuredContent": out, "isError": err != nil}
	default:
		sendJSON(w, 200, rpcError(req.ID, -32601, "Method not found"))
		return
	}
	sendJSON(w, 200, object{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func supportedMCP(version string) bool {
	return version == mcpProtocol || version == "2025-06-18" || version == "2025-03-26" || version == "2024-11-05"
}

func mcpObject(raw []byte) (object, error) {
	if len(raw) == 0 {
		return object{}, nil
	}
	return decodeObject(raw)
}

var mcpTools = func() []object {
	str := func(description string) object { return object{"type": "string", "description": description} }
	integer := func(description string, min, max int) object {
		return object{"type": "integer", "minimum": min, "maximum": max, "description": description}
	}
	array := func(items object) object { return object{"type": "array", "items": items} }
	schema := func(props object, required ...string) object {
		if required == nil {
			required = []string{}
		}
		return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	withID := func(props object, required ...string) object {
		props["device_id"] = str("Exact EchoEar device ID")
		return schema(props, append([]string{"device_id"}, required...)...)
	}
	tool := func(name, description string, in object) object {
		return object{"name": name, "description": description, "inputSchema": in}
	}
	bg := str("Background color as #RRGGBB")
	node := object{"id": str("Stable node ID (1–24 letters, numbers, dots, underscores or hyphens)"), "t": object{"type": "string", "enum": []string{"circle", "rect", "line", "arc", "text", "btn"}}}
	for _, key := range strings.Fields("x y x1 y1 x2 y2 w h r") {
		node[key] = object{"type": "integer", "description": "Pixel coordinate or dimension; clamped to 0–359"}
	}
	for _, key := range strings.Fields("a1 a2") {
		node[key] = integer("Angle in degrees", 0, 360)
	}
	for _, key := range strings.Fields("c bc tc") {
		node[key] = str("Color as #RRGGBB")
	}
	for _, key := range strings.Fields("fill vis") {
		node[key] = object{"type": "boolean"}
	}
	node["sw"], node["fs"] = integer("Stroke width", 0, 180), integer("Font scale", 1, 3)
	node["s"], node["on"] = str("Display text"), object{"type": "string", "enum": []string{"tap", ""}}
	node["action"] = object{"type": "string", "enum": []string{"exit", ""}, "description": "Optional tap-back to the face"}
	node["ask"], node["tap"] = str("Optional fallback prompt if no waiter"), str("Optional spoken feedback if no waiter")
	fields := maps.Clone(node)
	delete(fields, "id")
	return []object{
		tool("devices", "List physical and virtual EchoEars and their connection, mode and cursor.", schema(object{})),
		tool("snapshot", "Read the device status, 360×360 keyed scene and last 20 events.", withID(object{})),
		tool("scene", "Replace the entire screen or restore the cat face. Draw in node order on a 360×360 canvas.",
			withID(object{"bg": bg, "title": str("Optional short title shown at the top"), "nodes": array(schema(node, "id", "t")),
				"preset": object{"type": "string", "enum": []string{"face"}}})),
		tool("patch", "Apply a keyed scene delta; snapshot again if expected_rev conflicts.",
			withID(object{"bg": bg, "expected_rev": integer("Expected scene revision", 0, 1<<32-1),
				"set": object{"type": "object", "additionalProperties": schema(fields)}, "add": array(schema(node, "id", "t")), "del": array(str("Node ID"))})),
		tool("speak", "Speak plain text on the device. Do not repeat these words in your final reply.", withID(object{"text": str("Plain words to speak")}, "text")),
		tool("chime", "Play a short sound on the device.", withID(object{"kind": object{"type": "string", "enum": []string{"tap", "think", "intercom", "celebrate"}}}, "kind")),
		tool("intercom", "Send a spoken message and screen card to an explicit online peer. Use all only if asked.",
			schema(object{"from_device_id": str("Sender ID"), "to_device_id": str("Recipient ID or all"), "text": str("Message to speak")}, "from_device_id", "to_device_id", "text")),
		tool("wait_event", "Wait for the next tap, head_pet, speech or intercom after the cursor. If timeout, event is null.",
			withID(object{"after_cursor": integer("Last cursor seen; defaults to current cursor", 0, 1<<53-1),
				"timeout_ms": integer("Defaults to 15000; clamped to 60000", 0, 60000)})),
	}
}()

type deviceEvent struct {
	Cursor    uint64    `json:"cursor"`
	Type      string    `json:"type"`
	DeviceID  string    `json:"device_id"`
	Timestamp time.Time `json:"timestamp"`
	Data      object    `json:"data"`
}

type eventRing struct {
	cursor uint64
	recent [eventLimit]deviceEvent
	subs   map[chan deviceEvent]eventSubscription
}

type eventSubscription struct {
	after       uint64
	ctx, device context.Context
}

func (s eventSubscription) active() bool { return s.ctx.Err() == nil && s.device.Err() == nil }

func (h *wsHub) hasWaiter(d *wsDevice) bool {
	if e := h.events[d.key]; e != nil {
		for _, sub := range e.subs {
			if sub.active() {
				return true
			}
		}
	}
	return false
}

func (h *wsHub) ring(key string) *eventRing {
	e := h.events[key]
	if e == nil {
		e = &eventRing{subs: make(map[chan deviceEvent]eventSubscription)}
		h.events[key] = e
	}
	return e
}

func (h *wsHub) emit(id, kind string, data object) bool {
	key := strings.ToLower(id)
	e := h.ring(key)
	e.cursor++
	event := deviceEvent{e.cursor, kind, id, time.Now().UTC(), data}
	e.recent[(e.cursor-1)%eventLimit] = event
	waiting := false
	for ch, sub := range e.subs {
		if !sub.active() {
			continue
		}
		waiting = true
		if event.Cursor > sub.after {
			select {
			case ch <- event:
			default:
			}
		}
	}
	return waiting
}

func (b *bridge) deviceEvent(id, kind string, data object) {
	if b.ws != nil {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		b.ws.emit(id, kind, data)
	}
}

func (e *eventRing) last(n uint64) []deviceEvent {
	first := e.cursor - min(e.cursor, n)
	out := make([]deviceEvent, 0, e.cursor-first)
	for i := first; i < e.cursor; i++ {
		out = append(out, e.recent[i%eventLimit])
	}
	return out
}

func (h *wsHub) waitEvent(ctx context.Context, id string, after *uint64, timeout time.Duration) (object, error) {
	key := strings.ToLower(id)
	h.mu.Lock()
	e := h.ring(key)
	cursor := e.cursor
	if after != nil {
		cursor = *after
		for _, event := range e.last(eventLimit) {
			if event.Cursor > cursor {
				h.mu.Unlock()
				return object{"event": event, "event_cursor": event.Cursor}, nil
			}
		}
	}
	d := h.devices[key]
	if d == nil || !h.active(d) {
		h.mu.Unlock()
		return nil, problem(409, "Device is not connected")
	}
	ch := make(chan deviceEvent, 1)
	e.subs[ch] = eventSubscription{cursor, ctx, d.client.ctx}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(e.subs, ch)
		h.mu.Unlock()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-d.client.ctx.Done():
		return nil, problem(409, "Device disconnected")
	case <-timer.C:
		return object{"event": nil, "event_cursor": cursor}, nil
	case ev := <-ch:
		return object{"event": ev, "event_cursor": ev.Cursor}, nil
	}
}

func (b *bridge) lookupDevice(id string) (peer, error) {
	if v, ok := b.virtualByID(id); ok {
		return peer{DeviceID: v.ID, Character: v.Character}, nil
	}
	if virtualID.MatchString(strings.ToLower(id)) {
		return peer{}, problem(404, "Device not found")
	}
	others, err := b.peerSnapshot("")
	if err != nil {
		return peer{}, err
	}
	for _, p := range others {
		if p.DeviceID == id {
			return p, nil
		}
	}
	return peer{}, problem(404, "Device not found")
}

func (h *wsHub) deviceStatus(entry object, id string) {
	key := strings.ToLower(id)
	entry["ws_connected"], entry["ws"], entry["mode"] = false, false, "offline"
	entry["battery_pct"], entry["volume"], entry["scene_rev"], entry["event_cursor"] = nil, nil, uint64(0), uint64(0)
	if entry["online"] == true {
		entry["mode"] = "http"
	}
	if e := h.events[key]; e != nil {
		entry["event_cursor"] = e.cursor
	}
	if d := h.devices[key]; d != nil && h.active(d) {
		entry["ws_connected"], entry["ws"], entry["online"], entry["mode"] = true, true, true, d.pose.mood
		entry["volume"], entry["scene_rev"], entry["firmware"] = d.vol, d.rev, d.firmware
		if d.pct >= 0 {
			entry["battery_pct"] = d.pct
		}
	}
}

var mcpDeviceFields = []string{"device_id", "character", "kind", "online", "ws_connected", "mode", "battery_pct", "volume", "scene_rev", "event_cursor", "theme"}

func mcpDevice(entry object) object {
	out := make(object, len(mcpDeviceFields))
	for _, key := range mcpDeviceFields {
		out[key] = entry[key]
	}
	return out
}

func mcpUint(args object, key string) (*uint64, error) {
	if v, ok := args[key]; ok {
		n, valid := v.(json.Number)
		if valid {
			if num, err := strconv.ParseUint(string(n), 10, 64); err == nil {
				return &num, nil
			}
		}
		return nil, problem(400, "Invalid "+key)
	}
	return nil, nil
}

func (b *bridge) callMCP(ctx context.Context, name string, args object) (object, error) {
	if b.ws == nil {
		return nil, problem(503, "Device hub unavailable")
	}
	switch name {
	case "devices", "snapshot":
		devices, err := b.deviceList()
		if err != nil {
			return nil, err
		}
		if name == "devices" {
			for i, entry := range devices {
				devices[i] = mcpDevice(entry)
			}
			return object{"devices": devices}, nil
		}
		id := str(args["device_id"])
		for _, entry := range devices {
			if entry["device_id"] == id {
				h := b.ws
				h.mu.Lock()
				defer h.mu.Unlock()
				h.deviceStatus(entry, id)
				events := h.ring(strings.ToLower(id))
				view := sceneView{nodes: []object{}}
				var rev uint64
				custom := false
				if d := h.devices[strings.ToLower(id)]; d != nil && h.active(d) {
					view, rev, custom = d.scene, d.rev, d.customScene
				}
				return object{"device": mcpDevice(entry), "scene": object{"rev": rev, "bg": view.bg, "nodes": view.nodes, "custom_scene": custom},
					"event_cursor": events.cursor, "events": events.last(20)}, nil
			}
		}
		return nil, problem(404, "Device not found")
	case "intercom":
		return b.mcpIntercom(ctx, args)
	}
	id := str(args["device_id"])
	who, err := b.lookupDevice(id)
	if err != nil {
		return nil, err
	}
	switch name {
	case "scene", "patch":
		result, err := b.ws.mcpScene(id, name, args)
		if err == nil && name == "scene" {
			title := shortText(args["title"], 48, false)
			text := "Showed a scene"
			if args["preset"] == "face" {
				text = "Returned to the cat face"
			}
			entry := newChat("system", who.Character, text, "mcp")
			entry.SceneTitle, entry.ToolSummary = title, text
			b.chat(id, entry, true)
		}
		return result, err
	case "wait_event":
		after, err := mcpUint(args, "after_cursor")
		if err != nil {
			return nil, err
		}
		ms, err := mcpUint(args, "timeout_ms")
		if err != nil {
			return nil, err
		}
		timeout := 15 * time.Second
		if ms != nil {
			timeout = time.Duration(min(60000, *ms)) * time.Millisecond
		}
		return b.ws.waitEvent(ctx, id, after, timeout)
	case "speak":
		return b.mcpSpeak(ctx, who, str(args["text"]))
	case "chime":
		return b.mcpChime(ctx, who, str(args["kind"]))
	}
	return nil, problem(400, "Unknown tool")
}

type sceneConflict struct{ rev uint64 }

func (e *sceneConflict) Error() string {
	return fmt.Sprintf("Scene revision conflict; current rev is %d", e.rev)
}

func listNodes(v any) ([]object, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, problem(400, "Expected an array of nodes")
	}
	nodes := make([]object, 0, min(len(raw), maxNodes))
	for _, v := range raw[:min(len(raw), maxNodes)] {
		value, ok := v.(object)
		if !ok {
			return nil, problem(400, "Expected a node object")
		}
		nodes = append(nodes, value)
	}
	return nodes, nil
}

// Keep only scalar protocol fields; sceneDiff compares the values directly.
func customNode(raw object) (object, sceneAction, error) {
	id, t := str(raw["id"]), str(raw["t"])
	if !deviceIDRE.MatchString(id) || len(id) > 24 || id == exitNode {
		return nil, sceneAction{}, problem(400, "Invalid or reserved node ID")
	}
	if _, ok := shapeFields[t]; !ok {
		return nil, sceneAction{}, problem(400, "Unknown node type")
	}
	n := object{"id": id, "t": t}
	for _, key := range strings.Fields("x y x1 y1 x2 y2 w h r sw fs a1 a2") {
		value, exists := raw[key]
		if !exists {
			continue
		}
		limit := 359
		switch key {
		case "fs":
			limit = 3
		case "sw":
			limit = 180
		case "a1", "a2":
			limit = 360
		}
		num, ok := value.(int)
		if !ok {
			num, ok = coordinate(value, float64(limit))
		}
		if !ok {
			return nil, sceneAction{}, problem(400, "Invalid numeric node field "+key)
		}
		if key == "fs" {
			num = max(1, num)
		}
		n[key] = num
	}
	for _, key := range []string{"c", "tc", "bc"} {
		if value, exists := raw[key]; exists {
			tint := color(value)
			if tint == "" {
				return nil, sceneAction{}, problem(400, "Invalid node color")
			}
			n[key] = tint
		}
	}
	for _, key := range []string{"vis", "fill"} {
		if value, ok := raw[key].(bool); ok {
			n[key] = value
		}
	}
	if v, ok := raw["s"].(string); ok {
		n["s"] = shortText(v, 128, false)
	}
	action := sceneAction{}
	switch {
	case raw["action"] == "exit" && n["vis"] != false:
		action.kind = "cat"
	case shortText(raw["ask"], 120, true) != "":
		action = sceneAction{"ask", shortText(raw["ask"], 120, true)}
	case shortText(raw["tap"], 120, true) != "":
		action = sceneAction{"tap", shortText(raw["tap"], 120, true)}
	case raw["on"] == "tap" || t == "btn" && raw["on"] == nil:
		action.kind = "event"
	}
	if action.kind != "" {
		n["on"] = "tap"
	} else if raw["on"] == "" {
		n["on"] = ""
	}
	return n, action, nil
}

func (h *wsHub) mcpScene(id, method string, args object) (object, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) {
		return nil, problem(409, "Device is not connected")
	}
	rev, err := mcpUint(args, "expected_rev")
	if err != nil {
		return nil, err
	}
	if rev != nil && *rev != d.rev {
		return nil, &sceneConflict{d.rev}
	}
	now := time.Now()
	status := func() object {
		return object{"ok": true, "device_id": id, "rev": d.rev, "event_cursor": h.ring(d.key).cursor}
	}
	if preset, ok := args["preset"]; ok && method == "scene" {
		if preset != "face" {
			return nil, problem(400, "Unknown preset")
		}
		d.view, d.pending, d.drawer, d.customScene, d.agentSetScene = nil, nil, false, false, d.busy
		h.wake(d, now)
		h.render(d, now, true)
		return status(), nil
	}
	nodes, err := listNodes(args["nodes"])
	if err != nil {
		return nil, err
	}
	title, bg := shortText(args["title"], 48, false), "#0B1020"
	if method == "patch" {
		view := d.scene
		if d.customScene && d.view != nil {
			view = *d.view
		}
		bg = view.bg
		set, hasSet := args["set"].(object)
		if args["set"] != nil && !hasSet {
			return nil, problem(400, "Expected set keyed by node ID")
		}
		del, hasDel := args["del"].([]any)
		if args["del"] != nil && !hasDel {
			return nil, problem(400, "Expected del array of node IDs")
		}
		gone := map[string]bool{}
		for _, value := range del {
			key := str(value)
			if !deviceIDRE.MatchString(key) {
				return nil, problem(400, "Invalid node ID in del")
			}
			gone[key] = true
		}
		nodes = make([]object, 0, len(view.nodes))
		for _, n := range view.nodes {
			key := str(n["id"])
			if key == exitNode || gone[key] {
				continue
			}
			n = maps.Clone(n)
			a := view.actions[key]
			if fields, ok := set[key]; ok {
				values, ok := fields.(object)
				if !ok || values["id"] != nil {
					return nil, problem(400, "Expected a node field object without id in set")
				}
				if _, ok := values["action"]; ok {
					a = sceneAction{}
				}
				if _, ok := values["on"]; ok {
					a = sceneAction{}
				}
				if _, ok := values["ask"]; ok {
					a = sceneAction{}
				}
				if _, ok := values["tap"]; ok {
					a = sceneAction{}
				}
				maps.Copy(n, values)
			}
			if a.kind != "" {
				// Internal only; unmodified native hit actions survive keyed patches.
				n["sceneAction"] = a
			}
			nodes = append(nodes, n)
		}
		add, err := listNodes(args["add"])
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, add...)
	}
	if value, ok := args["bg"]; ok {
		bg = color(value)
		if bg == "" {
			return nil, problem(400, "Invalid scene background")
		}
	}
	next := sceneView{bg: bg, nodes: make([]object, 0, min(maxNodes, len(nodes)+2)), actions: make(map[string]sceneAction)}
	ids := make(map[string]bool)
	hasExit := false
	if title != "" {
		next.nodes = append(next.nodes, label("echoear-title", 180, 38, 2, title, "#FFF1C8"))
		ids["echoear-title"] = true
	}
	for _, raw := range nodes[:min(len(nodes), maxNodes-len(next.nodes))] {
		n, a, err := customNode(raw)
		if err != nil {
			return nil, err
		}
		key := str(n["id"])
		if old, ok := raw["sceneAction"].(sceneAction); ok {
			a = old
			n["on"] = raw["on"]
		}
		if ids[key] {
			return nil, problem(400, "Duplicate node ID "+key)
		}
		ids[key] = true
		next.nodes = append(next.nodes, n)
		if a.kind != "" {
			next.actions[key] = a
		}
		hasExit = hasExit || a.kind == "cat" && n["vis"] != false
	}
	if !hasExit {
		if len(next.nodes) == maxNodes {
			delete(next.actions, str(next.nodes[maxNodes-1]["id"]))
			next.nodes = next.nodes[:maxNodes-1]
		}
		next.nodes = append(next.nodes, comicButton(exitNode, 139, 300, 82, 32, "Cat", "#FFFAEE"))
		next.actions[exitNode] = sceneAction{"cat", ""}
	}
	d.view, d.pending, d.drawer, d.customScene, d.agentSetScene = &next, nil, false, true, d.busy
	h.wake(d, now)
	beforeRev, beforeView := d.rev, d.scene
	h.render(d, now, method == "scene")
	if d.rev == beforeRev && !maps.Equal(beforeView.actions, next.actions) {
		d.rev++
		h.push(d, object{"op": "patch", "rev": d.rev}, true)
	}
	return status(), nil
}

func (b *bridge) mcpSpeak(ctx context.Context, who peer, text string) (object, error) {
	clean, err := spokenText(text)
	if err != nil {
		return nil, err
	}
	if b.worker == nil || !b.worker.ready.Load() {
		return nil, problem(503, "TTS unavailable")
	}
	h := b.ws
	key := strings.ToLower(who.DeviceID)
	h.mu.Lock()
	device := h.devices[key]
	httpSpoke := h.httpSpeech[key]
	var turn uint64
	if device != nil {
		turn = device.turn
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, ttsWait)
	defer cancel()
	pcm, err := b.worker.synthesize(ctx, clean)
	if err != nil {
		return nil, problem(502, "TTS synthesis failed")
	}
	h.mu.Lock()
	d := h.devices[key]
	if d != nil && h.active(d) {
		if device != d || d.turn != turn {
			h.mu.Unlock()
			return nil, problem(409, "Device turn changed while synthesizing")
		}
		if d.busy {
			d.agentSpoke = true
		}
		stream := newAudioStream()
		stream.append(pcm)
		stream.finish(nil)
		h.wake(d, time.Now())
		h.play(d, d.client.ctx, stream)
		h.mu.Unlock()
	} else {
		h.mu.Unlock()
		if who.Port == 0 || !b.postPeer(ctx, who, "/speak", pcmType, pcm) {
			return nil, problem(502, "Device speech delivery failed")
		}
	}
	if httpSpoke != nil {
		httpSpoke.Store(true)
	}
	b.chat(who.DeviceID, newChat("assistant", who.Character, clean, "mcp"), true)
	return object{"ok": true, "device_id": who.DeviceID}, nil
}

func (b *bridge) mcpChime(ctx context.Context, who peer, kind string) (object, error) {
	if kind != "tap" && kind != "think" && kind != "intercom" && kind != "celebrate" {
		return nil, problem(400, "Unknown chime kind")
	}
	h := b.ws
	h.mu.Lock()
	if d := h.devices[strings.ToLower(who.DeviceID)]; d != nil && h.active(d) {
		wire := kind
		if kind == "celebrate" {
			wire = "intercom"
		} // Existing firmware's happy tone.
		h.push(d, object{"op": "chime", "kind": wire}, false)
		h.mu.Unlock()
	} else {
		h.mu.Unlock()
		if who.Port == 0 || !b.postPeer(ctx, who, "/speak", pcmType, chimePCM(kind)) {
			return nil, problem(502, "Device chime delivery failed")
		}
	}
	return object{"ok": true, "device_id": who.DeviceID, "kind": kind}, nil
}

func (b *bridge) mcpIntercom(ctx context.Context, args object) (object, error) {
	sender, err := b.lookupDevice(str(args["from_device_id"]))
	if err != nil {
		return nil, err
	}
	text := shortText(args["text"], maxMessage, true)
	if _, err := spokenText(text); err != nil {
		return nil, err
	}
	to := str(args["to_device_id"])
	others, err := b.peerSnapshot(sender.DeviceID)
	if err != nil {
		return nil, err
	}
	online := slices.DeleteFunc(others, func(p peer) bool {
		return time.Since(time.Unix(p.LastSeen, 0)) > 180*time.Second || to != "all" && p.DeviceID != to
	})
	if len(online) == 0 {
		return nil, problem(404, "Online recipient not found")
	}
	out := b.deliver(ctx, sender, online, text, nil)
	if len(out.sent) == 0 {
		return nil, problem(502, "Intercom delivery failed")
	}
	return object{"ok": true, "sent_to": out.sent, "delivered_to": out.delivered, "failed_to": out.failed}, nil
}
