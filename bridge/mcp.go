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
			"instructions": "Use the exact EchoEar device_id from the conversation. Use scene/patch tap and cycle for local interaction, with move and revert_ms for motion. Reserve wait_event for input needed by the current turn; pass the latest event_cursor."}
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
			var version *versionConflict
			if errors.As(err, &version) {
				out["version"] = version.version
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
	say := object{"type": "string", "maxLength": maxText, "description": "Speak plain text on the device without repeating it in the final reply"}
	chime := object{"type": "string", "enum": []string{"tap", "think", "intercom", "celebrate"}}
	ref := func(name string) object { return object{"$ref": "#/$defs/" + name} }
	node := object{"id": str("Stable node ID (1–24 letters, numbers, dots, underscores or hyphens)"),
		"t": object{"type": "string", "enum": []string{"circle", "rect", "line", "tri", "arc", "text", "btn"}}}
	for _, key := range strings.Fields("x y x1 y1 x2 y2 x3 y3 w h r") {
		node[key] = object{"type": "integer", "description": "Pixel coordinate or dimension; clamped to 0–359"}
	}
	for _, key := range strings.Fields("a1 a2") {
		node[key] = integer("Angle in degrees", 0, 360)
	}
	for _, key := range strings.Fields("c bc tc") {
		node[key] = str("Color as #RRGGBB")
	}
	node["vis"] = object{"type": "boolean"}
	node["fill"] = object{"type": "boolean", "description": "Fill the shape; defaults to true for tri"}
	node["group"] = object{"type": []string{"string", "null"}, "description": "Name for moving nodes together; null clears it"}
	node["sw"], node["fs"] = integer("Stroke width", 0, 180), integer("Font scale", 1, 3)
	node["s"], node["on"] = str("Plain ASCII display text (no emoji)"), object{"type": "string", "enum": []string{"tap", ""}}
	node["action"] = object{"type": "string", "enum": []string{"exit", ""}, "description": "Optional tap-back to the face"}
	node["ask"] = str("Legacy fallback prompt if no waiter; prefer tap.ask")
	node["tap"] = object{"description": `Local reaction; auto-enables taps. Example: {"move":{"target":"fish","dx":0,"dy":-90},"revert_ms":400}. Null removes it.`,
		"anyOf": []object{ref("reaction"), str("Legacy spoken feedback"), {"type": "null"}}}
	node["cycle"] = object{"description": "Rotate through 2–8 local reactions; null removes it.", "anyOf": []object{
		{"type": "array", "items": ref("reaction"), "minItems": 2, "maxItems": 8}, {"type": "null"},
	}}
	extra := maps.Clone(node)
	for _, key := range strings.Fields("id vis group on action ask tap cycle tc") {
		delete(extra, key)
	}
	extra["t"] = object{"enum": []string{"rect", "circle", "line", "arc", "tri", "text"}}
	extras := array(schema(extra, "t", "c"))
	extras["maxItems"] = maxCatExtras
	fields := maps.Clone(node)
	delete(fields, "id")
	nodes := array(ref("node"))
	nodes["maxItems"] = maxNodes
	patch := object{"bg": bg, "set": object{"type": "object", "additionalProperties": schema(fields),
		"description": `Partial updates keyed by exact existing node ID. Lines use two coordinate pairs, triangles use three.`},
		"add": nodes, "del": array(str("Node ID"))}
	move := schema(object{"target": str("Exact node ID or group name"), "dx": object{"type": "integer"}, "dy": object{"type": "integer"}}, "target", "dx", "dy")
	reaction := schema(object{"chime": chime, "speak": object{"type": "string", "maxLength": 240, "description": "Speak locally without the AI"},
		"patch": ref("patch"), "state": str("Switch to this named state before applying patch or move"),
		"move": move, "revert_ms": integer("Undo visual attributes changed by move or patch.set, and any background change", 50, 10000),
		"after_ms": integer("Delay before then; required with then, at most two chained delays", 50, 10000), "then": ref("reaction"),
		"ask": object{"type": "string", "maxLength": 240, "description": "Start a new AI turn only when needed"}, "exit": object{"type": "boolean"},
		"broadcast": object{"type": "boolean", "description": "Apply the local visual/audio reaction to all connected cats in the current room"},
		"store":     schema(object{"scope": str("shared, a device ID, or game:<slug>"), "key": str("Memory key"), "value": object{}}, "scope", "key", "value")})
	reaction["dependentRequired"] = object{"after_ms": []string{"then"}, "then": []string{"after_ms"}}
	withSceneDefs := func(in object) object {
		in["$defs"] = object{"node": schema(node, "id", "t"), "reaction": reaction, "patch": schema(patch)}
		return in
	}
	title := str("Optional short title shown at the top")
	states := object{"type": "object", "description": "Named screens selected by tap.state", "maxProperties": maxSceneStates, "additionalProperties": schema(object{"bg": bg, "title": title, "nodes": nodes}, "nodes")}
	patchArgs := maps.Clone(patch)
	patchArgs["expected_rev"] = integer("Expected scene revision", 0, 1<<32-1)
	return []object{
		tool("devices", "List physical and virtual EchoEars and their connection, mode and cursor.", schema(object{})),
		tool("snapshot", "Read the device status, 360×360 keyed scene and last 20 events.", withID(object{})),
		tool("scene", "Draw geometric shapes and ASCII text on a 360×360 screen, or restore the cat face. Use tap and cycle for local reactions; move a node or group and undo with revert_ms. Use say or chime for immediate audio.",
			withSceneDefs(withID(object{"bg": bg, "title": title, "nodes": nodes, "states": states, "chime": chime, "say": say,
				"room": str("Join a live multiplayer room by slug"), "asset": str("Load saved visuals by name"), "save_as": str("Save sanitized visuals as this asset name"),
				"preset": object{"type": "string", "enum": []string{"face"}}}))),
		tool("patch", "Update visuals or local reactions by node ID; snapshot again if expected_rev conflicts.",
			withSceneDefs(withID(patchArgs))),
		tool("speak", "Speak plain text on the device. Do not repeat these words in your final reply.", withID(object{"text": str("Plain words to speak")}, "text")),
		tool("chime", "Play a short sound on the device.", withID(object{"kind": chime}, "kind")),
		tool("intercom", "Send a spoken message and screen card to an explicit online peer. Use all only if asked.",
			schema(object{"from_device_id": str("Sender ID"), "to_device_id": str("Recipient ID or all"), "text": str("Message to speak")}, "from_device_id", "to_device_id", "text")),
		tool("control", "Set a connected device's volume, put it to sleep, or wake it. Supply volume, sleep, or both.",
			withID(object{"volume": integer("Speaker volume", 0, 100), "sleep": object{"type": "boolean", "description": "True dims and sleeps; false wakes"}})),
		tool("cat", "Get or edit a persistent animated cat. Use device_id for get, update, load_saved, reset_blank; use slug for load_saved or delete_saved. List saved cats without a device.",
			schema(object{"op": object{"enum": []string{"get", "update", "list_saved", "load_saved", "reset_blank", "delete_saved"}},
				"device_id": str("Exact EchoEar device ID"), "slug": str("Saved cat slug"), "name": object{"type": "string", "maxLength": 40},
				"face_bg": bg, "mark_color": str("Forehead mark color as #RRGGBB"), "accent_color": str("UI accent as #RRGGBB"),
				"personality": object{"type": "string", "maxLength": 160}, "voice": str("Kokoro voice ID (e.g. af_heart, bm_fable)"),
				"extras": extras, "configured": object{"type": "boolean"}, "save_profile": object{"type": "boolean"}}, "op")),
		tool("memory", "List, get, set or delete persistent JSON for a device, shared, or a game room. Use expected_version to prevent lost updates.",
			schema(object{"op": object{"enum": []string{"list", "get", "set", "delete"}}, "scope": str("shared, a device ID, or game:<slug>"),
				"key": str("Memory key"), "value": object{}, "expected_version": integer("Zero requires a new key; otherwise require this version", 0, 1<<53-1)}, "op", "scope")),
		tool("asset", "Save, list, get or delete reusable scene visuals and local reactions by name.",
			withSceneDefs(schema(object{"op": object{"enum": []string{"save", "list", "get", "delete"}}, "name": str("Asset slug"),
				"kind": object{"enum": []string{"scene", "nodes"}}, "author": str("Optional author"), "bg": bg, "title": title, "nodes": nodes, "states": states}, "op"))),
		tool("timer", "Set, list or cancel restart-safe timers. Default firing is local; wake_agent starts a new bounded turn instead.",
			withID(object{"op": object{"enum": []string{"set", "list", "cancel"}}, "id": str("Timer ID to cancel"), "all": object{"type": "boolean"},
				"in_seconds": integer("Delay in seconds; at least 30 for wake_agent", 1, 86400), "at": str("RFC3339 time within 24 hours, instead of in_seconds"),
				"label": str("Short timer label"), "say": say, "chime": chime, "wake_agent": object{"type": "boolean"}}, "op")),
		tool("wait_event", "Wait for input needed by the current turn; use local reactions for regular controls. On timeout or cancellation, event is null.",
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

func (h *wsHub) cancelWaiters(key string) {
	if e := h.events[key]; e != nil {
		for ch := range e.subs {
			delete(e.subs, ch)
			close(ch)
		}
	}
}

func (h *wsHub) cancelSceneUI(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := strings.ToLower(id)
	h.cancelWaiters(key)
	if d := h.devices[key]; d != nil {
		h.stopSceneTimers(d)
	}
}

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
	case ev, ok := <-ch:
		if d.client.ctx.Err() != nil {
			return nil, problem(409, "Device disconnected")
		}
		if !ok {
			return object{"event": nil, "event_cursor": cursor, "cancelled": true}, nil
		}
		return object{"event": ev, "event_cursor": ev.Cursor}, nil
	}
}

func (b *bridge) lookupDevice(id string) (peer, error) {
	if v, ok := b.virtualByID(id); ok {
		who := peer{DeviceID: v.ID, Character: v.Character}
		who.Character = b.catFor(who).Name
		return who, nil
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
	entry["room"] = ""
	entry["battery_pct"], entry["volume"], entry["scene_rev"], entry["event_cursor"] = nil, nil, uint64(0), uint64(0)
	if entry["online"] == true {
		entry["mode"] = "http"
	}
	if e := h.events[key]; e != nil {
		entry["event_cursor"] = e.cursor
	}
	if d := h.devices[key]; d != nil && h.active(d) {
		entry["ws_connected"], entry["ws"], entry["online"], entry["mode"] = true, true, true, d.pose.mood
		entry["character"], entry["cat"] = d.cat.Name, d.cat.catLook
		_, rev := d.sceneState()
		entry["volume"], entry["scene_rev"], entry["firmware"] = d.vol, rev, d.firmware
		entry["room"] = d.room
		if d.pct >= 0 {
			entry["battery_pct"] = d.pct
		}
	}
}

var mcpDeviceFields = []string{"device_id", "character", "cat", "kind", "online", "ws_connected", "mode", "battery_pct", "volume", "scene_rev", "event_cursor", "theme", "room"}

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
	switch name {
	case "cat":
		return b.catTool(ctx, args)
	case "memory":
		return b.memory(ctx, args, false)
	case "asset":
		return b.asset(ctx, args)
	case "timer":
		return b.timerMCP(ctx, args)
	}
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
					view, rev = d.sceneState()
					custom = d.customScene
				}
				return object{"device": mcpDevice(entry), "scene": object{"rev": rev, "bg": view.bg, "nodes": view.snapshotNodes(), "custom_scene": custom},
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
		var say, chime string
		if name == "scene" {
			if _, present := args["asset"]; present {
				if args["preset"] != nil {
					return nil, problem(400, "Use asset or preset, not both")
				}
				args, err = b.sceneAsset(ctx, args)
				if err != nil {
					return nil, err
				}
			}
			if raw, ok := args["say"]; ok {
				say, err = spokenText(str(raw))
				if err != nil {
					return nil, err
				}
			}
			if raw, ok := args["chime"]; ok {
				chime = str(raw)
				if !validChime(chime) {
					return nil, problem(400, "Unknown chime kind")
				}
			}
		}
		result, err := b.ws.mcpScene(ctx, id, name, args, chime)
		if err == nil && name == "scene" {
			title := shortText(args["title"], 48, false)
			text := "Showed a scene"
			if args["preset"] == "face" {
				text = "Returned to the cat face"
			}
			entry := newChat("system", who.Character, text, "mcp")
			entry.SceneTitle, entry.ToolSummary = title, text
			b.chat(id, entry, true)
			if say != "" {
				_, err = b.speakMCP(ctx, who, say)
			}
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
	case "control":
		return b.ws.mcpControl(id, args)
	}
	return nil, problem(400, "Unknown tool")
}

func (h *wsHub) mcpControl(id string, args object) (object, error) {
	volume, err := mcpUint(args, "volume")
	if err != nil || volume != nil && *volume > 100 {
		return nil, problem(400, "Volume must be an integer from 0 to 100")
	}
	sleep, setSleep := args["sleep"]
	asleep, isBool := sleep.(bool)
	if setSleep && !isBool {
		return nil, problem(400, "Sleep must be a boolean")
	}
	if volume == nil && !setSleep {
		return nil, problem(400, "Expected volume or sleep")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) {
		return nil, problem(409, "Device is not connected")
	}
	now := time.Now()
	if volume != nil {
		h.volume(d, int(*volume), now)
	}
	if setSleep {
		d.last = now
		h.setSleeping(d, asleep)
		h.render(d, now, false)
	}
	return object{"ok": true, "device_id": d.who.DeviceID, "volume": d.vol, "sleeping": d.asleep}, nil
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
func visualNode(raw object, partial bool) (object, error) {
	n := object{}
	if id := str(raw["id"]); !partial {
		if !validSceneID(id) || id == exitNode || id == "echoear-title" {
			return nil, problem(400, "Invalid or reserved node ID")
		}
		n["id"] = id
	}
	if t, exists := raw["t"]; !partial || exists {
		if _, ok := shapeFields[str(t)]; !ok {
			return nil, problem(400, "Unknown node type")
		}
		n["t"] = t
	}
	for _, key := range strings.Fields("x y x1 y1 x2 y2 x3 y3 w h r sw fs a1 a2") {
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
		if ok {
			num = min(limit, max(0, num))
		} else {
			num, ok = coordinate(value, float64(limit))
		}
		if !ok {
			return nil, problem(400, "Invalid numeric node field "+key)
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
				return nil, problem(400, "Invalid node color")
			}
			n[key] = tint
		}
	}
	for _, key := range []string{"vis", "fill"} {
		if value, ok := raw[key].(bool); ok {
			n[key] = value
		}
	}
	if !partial && n["t"] == "tri" && n["fill"] == nil {
		n["fill"] = true
	}
	if v, ok := raw["s"].(string); ok {
		n["s"] = shortText(v, 128, false)
	}
	return n, nil
}

func (h *wsHub) mcpScene(ctx context.Context, id, method string, args object, chime string) (object, error) {
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
	if _, current := d.sceneState(); rev != nil && *rev != current {
		return nil, &sceneConflict{current}
	}
	status := func() object {
		if chime != "" {
			h.chime(d, chime)
		}
		_, rev := d.sceneState()
		return object{"ok": true, "device_id": id, "rev": rev, "room": d.room, "event_cursor": h.ring(d.key).cursor}
	}
	now := time.Now()
	if preset, ok := args["preset"]; ok && method == "scene" {
		if preset != "face" {
			return nil, problem(400, "Unknown preset")
		}
		if args["save_as"] != nil || args["room"] != nil {
			return nil, problem(400, "The face preset cannot be saved or join a room")
		}
		h.clearView(d)
		d.agentSetScene = d.busy
		h.wake(d, now)
		h.render(d, now, true)
		return status(), nil
	}
	if method == "patch" {
		patch, err := parseScenePatch(args, d.states, 0)
		if err != nil {
			return nil, err
		}
		if err := h.patchScene(d, patch, nil, now); err != nil {
			return nil, err
		}
		return status(), nil
	}
	view, states, err := parseScene(args)
	if err != nil {
		return nil, err
	}
	room := str(args["room"])
	if _, supplied := args["room"]; supplied && !slugRE.MatchString(room) {
		return nil, problem(400, "Invalid room slug")
	}
	if name, supplied := args["save_as"]; supplied {
		if _, err := h.b.saveAsset(ctx, str(name), "scene", d.who.Character, view, states); err != nil {
			return nil, err
		}
	}
	h.clearLocalScene(d)
	d.room = room
	d.states = states
	h.showCustomScene(d, view, now, true)
	return status(), nil
}

func (b *bridge) mcpSpeak(ctx context.Context, who peer, text string) (object, error) {
	clean, err := spokenText(text)
	if err != nil {
		return nil, err
	}
	return b.speakMCP(ctx, who, clean)
}

func (b *bridge) speakMCP(ctx context.Context, who peer, clean string) (object, error) {
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
	pcm, err := b.worker.synthesize(ctx, b.voiceFor(who), clean)
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

func validChime(kind string) bool {
	return kind == "tap" || kind == "think" || kind == "intercom" || kind == "celebrate"
}

func (h *wsHub) chime(d *wsDevice, kind string) {
	if kind == "celebrate" {
		kind = "intercom" // Existing firmware's happy tone.
	}
	h.push(d, object{"op": "chime", "kind": kind}, false)
}

func (b *bridge) mcpChime(ctx context.Context, who peer, kind string) (object, error) {
	if !validChime(kind) {
		return nil, problem(400, "Unknown chime kind")
	}
	h := b.ws
	h.mu.Lock()
	if d := h.devices[strings.ToLower(who.DeviceID)]; d != nil && h.active(d) {
		h.chime(d, kind)
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
