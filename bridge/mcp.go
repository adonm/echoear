package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			"serverInfo":   object{"name": "echoear", "version": "2.0.0"},
			"instructions": "Use the exact EchoEar device ID. Inspect live context, run an SVG drawing or sandboxed JS app, act on a device, or store persistent JSON. Only final assistant prose is spoken."}
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
	text, number, flag := object{"type": "string"}, object{"type": "integer"}, object{"type": "boolean"}
	enum := func(items ...string) object { return object{"type": "string", "enum": items} }
	array := func(items object) object { return object{"type": "array", "items": items} }
	schema := func(props object, required ...string) object {
		if required == nil {
			required = []string{}
		}
		return object{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	tool := func(name, description string, in object) object {
		return object{"name": name, "description": description, "inputSchema": in}
	}
	notes := object{"anyOf": []object{text, array(text)}, "description": "PITCH:MS separated by spaces or an array, e.g. C5:150 E5:150"}
	return []object{
		tool("inspect", "Read live cat, clock, peers, screen, app, timers, memory keys, and saved cats/apps. Omit device_id for discovery.",
			schema(object{"device_id": text})),
		tool("run_app", "Display a 360×360 SVG, run a sandboxed JS app, load/save by slug, or exit back to the cat face. One shared app state across devices, render(state, player) per device.",
			schema(object{"devices": object{"anyOf": []object{text, array(text)}}, "title": text, "svg": text, "code": object{"type": "string", "maxLength": 16384},
				"load": text, "save_as": text, "tick_ms": number, "max_ticks": number, "exit": flag}, "devices")),
		tool("act", "Send a spoken intercom, play a melody/chime, change device controls, set/cancel a timer, or design/load a cat.",
			schema(object{"to": text, "intercom": schema(object{"from": text, "text": text}, "from", "text"), "notes": notes, "chime": enum("tap", "think", "intercom", "celebrate"),
				"volume": object{"type": "integer", "minimum": 0, "maximum": 100}, "sleep": flag,
				"timer": schema(object{"op": enum("set", "cancel"), "id": text, "all": flag, "in_seconds": number, "at": text, "label": text, "say": text, "notes": notes, "wake_agent": flag}, "op"),
				"cat": schema(object{"op": enum("update", "load", "reset_blank", "delete_saved"), "name": text, "face_bg": text, "mark_color": text, "accent_color": text,
					"personality": text, "voice": text, "svg_extras": text, "extras": array(object{"type": "object"}), "slug": text, "configured": flag}, "op")}, "to")),
		tool("store", "Read/write persistent JSON in a device, shared, game:<name>, or app:<name> scope. expected_version is atomic; zero means no key exists.",
			schema(object{"op": enum("get", "set", "delete", "list"), "scope": text, "key": text, "value": object{}, "expected_version": number}, "op", "scope")),
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
		if d.board != "" {
			entry["board"], entry["shape"], entry["width"], entry["height"] = d.board, d.screen.shape, d.screen.width, d.screen.height
		}
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
	case "inspect":
		return b.inspect(ctx, str(args["device_id"]))
	case "run_app":
		return b.runApp(ctx, args)
	case "act":
		return b.act(ctx, args)
	case "store":
		return b.memory(ctx, args, false)
	default:
		return nil, problem(400, "Unknown tool")
	}
}

func (b *bridge) inspect(ctx context.Context, id string) (object, error) {
	devices, err := b.deviceList()
	if err != nil {
		return nil, err
	}
	out := object{"me": nil, "now": time.Now().Format(time.RFC3339), "peers": []object{}, "screen": nil, "active_app": nil,
		"timers": []savedTimer{}, "memory_keys": []object{}, "saved_cats": []object{}, "saved_apps": []object{}}
	list := []object{}
	for _, entry := range devices {
		p := object{"device_id": entry["device_id"], "name": entry["character"], "online": entry["online"], "kind": entry["kind"]}
		if str(entry["device_id"]) == id {
			p["mode"], p["volume"], p["battery_pct"], p["ws_connected"] = entry["mode"], entry["volume"], entry["battery_pct"], entry["ws_connected"]
			out["me"] = p
		} else {
			list = append(list, p)
		}
	}
	if id != "" {
		if out["me"] == nil {
			return nil, problem(404, "Device not found")
		}
		who, err := b.lookupDevice(id)
		if err != nil {
			return nil, err
		}
		out["me"].(object)["cat"] = b.catFor(who)
		if h := b.ws; h != nil {
			h.mu.Lock()
			if d := h.devices[strings.ToLower(id)]; d != nil && h.active(d) {
				title := d.scene.title
				if title == "" {
					title = "Cat"
				}
				hits := []string{}
				for _, hit := range d.svg.hits {
					if !slices.Contains(hits, hit.id) {
						hits = append(hits, hit.id)
					}
				}
				out["screen"] = object{"title": title, "rev": d.rev, "hit_ids": hits}
				if a := d.app; a != nil {
					players := []string{}
					for _, p := range a.devices {
						if p != nil && h.active(p) {
							players = append(players, p.who.DeviceID)
						}
					}
					out["active_app"] = object{"name": a.spec.Name, "title": a.spec.Title, "kind": a.spec.Kind, "devices": players, "ticks_left": a.remaining}
				}
			}
			h.mu.Unlock()
		}
	}
	out["peers"] = list
	if b.timers != nil {
		out["timers"], err = b.timers.list(ctx, id)
		if err != nil {
			return nil, err
		}
	}
	if b.db != nil {
		if id != "" {
			for _, scope := range []string{id, "shared"} {
				m, err := b.memory(ctx, object{"op": "list", "scope": scope}, false)
				if err != nil {
					return nil, err
				}
				out["memory_keys"] = append(out["memory_keys"].([]object), object{"scope": scope, "entries": m["entries"]})
			}
		}
		cats, err := b.catTool(ctx, object{"op": "list_saved"})
		if err != nil {
			return nil, err
		}
		for _, c := range cats["cats"].([]savedCat) {
			out["saved_cats"] = append(out["saved_cats"].([]object), object{"slug": c.Slug, "name": c.Name})
		}
		out["saved_apps"], err = b.listApps(ctx)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *bridge) act(ctx context.Context, args object) (object, error) {
	to := str(args["to"])
	if !deviceIDRE.MatchString(to) {
		return nil, problem(400, "Expected exact device ID or all")
	}
	if to == "all" && (len(args) != 2 || args["intercom"] == nil) {
		return nil, problem(400, "Use all only for intercom")
	}
	out := object{"ok": true, "to": to}
	var pcm []byte
	kind := str(args["chime"])
	if raw, ok := args["chime"]; ok && !validChime(str(raw)) {
		return nil, fieldError("chime", "unknown kind")
	}
	if raw, ok := args["notes"]; ok {
		_, p, err := melodyInput(raw, "notes")
		if err != nil {
			return nil, err
		}
		pcm = p
	}
	if args["volume"] != nil || args["sleep"] != nil {
		if b.ws == nil {
			return nil, problem(503, "Device hub unavailable")
		}
		r, err := b.ws.mcpControl(to, args)
		if err != nil {
			return nil, err
		}
		out["volume"], out["sleeping"] = r["volume"], r["sleeping"]
	}
	if raw, ok := args["cat"]; ok {
		c, ok := raw.(object)
		if !ok {
			return nil, fieldError("cat", "expected an object")
		}
		c = withDevice(c, to)
		switch str(c["op"]) {
		case "load":
			c["op"] = "load_saved"
			if str(c["slug"]) == "" {
				c["slug"] = catSlug(str(c["name"]))
			}
		case "delete_saved":
			if str(c["slug"]) == "" {
				c["slug"] = catSlug(str(c["name"]))
			}
		case "update", "reset_blank":
		default:
			return nil, fieldError("cat.op", "unknown operation")
		}
		r, err := b.catTool(ctx, c)
		if err != nil {
			return nil, err
		}
		out["cat"] = r["cat"]
	}
	if raw, ok := args["timer"]; ok {
		t, ok := raw.(object)
		if !ok || t["op"] != "set" && t["op"] != "cancel" {
			return nil, fieldError("timer", "expected op set or cancel")
		}
		r, err := b.timerMCP(ctx, withDevice(t, to))
		if err != nil {
			return nil, err
		}
		out["timer"] = r
	}
	if raw, ok := args["intercom"]; ok {
		i, ok := raw.(object)
		if !ok {
			return nil, fieldError("intercom", "expected from and text")
		}
		r, err := b.mcpIntercom(ctx, object{"from_device_id": i["from"], "to_device_id": to, "text": i["text"]})
		if err != nil {
			return nil, err
		}
		out["intercom"] = r
	}
	if kind != "" || len(pcm) != 0 {
		who, err := b.lookupDevice(to)
		if err != nil {
			return nil, err
		}
		if _, err := b.mcpChime(ctx, who, kind, pcm); err != nil {
			return nil, err
		}
		out["played"] = true
	}
	if len(out) == 2 {
		return nil, problem(400, "Expected an action")
	}
	return out, nil
}

func (b *bridge) callLegacyMCP(ctx context.Context, name string, args object) (object, error) {
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
		var pcm []byte
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
			if raw, ok := args["notes"]; ok {
				if say != "" {
					return nil, fieldError("notes", "use notes or say, not both")
				}
				_, pcm, err = parseMelody(raw, "notes")
				if err != nil {
					return nil, err
				}
			}
		}
		result, err := b.ws.mcpScene(ctx, id, name, args, chime, pcm)
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
		var pcm []byte
		if raw, ok := args["notes"]; ok {
			_, pcm, err = parseMelody(raw, "notes")
			if err != nil {
				return nil, err
			}
		}
		return b.mcpChime(ctx, who, str(args["kind"]), pcm)
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
	return listNodesAt(v, "nodes")
}

func listNodesAt(v any, path string) ([]object, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, fieldError(path, "expected an array of nodes")
	}
	nodes := make([]object, 0, min(len(raw), maxNodes))
	for i, v := range raw[:min(len(raw), maxNodes)] {
		value, ok := v.(object)
		if !ok {
			return nil, fieldError(fmt.Sprintf("%s[%d]", path, i), "expected a node object")
		}
		nodes = append(nodes, value)
	}
	return nodes, nil
}

// Keep only scalar protocol fields; sceneDiff compares the values directly.
func visualNode(raw object, partial bool) (object, error) {
	return visualNodeAt(raw, partial, "node")
}

func visualNodeAt(raw object, partial bool, path string) (object, error) {
	n := object{}
	if id := str(raw["id"]); !partial {
		if !validSceneID(id) || id == exitNode || id == "echoear-title" {
			return nil, fieldError(path+".id", "use an unreserved node ID (1–24 letters, numbers, dots, underscores or hyphens)")
		}
		n["id"] = id
	}
	if t, exists := raw["t"]; !partial || exists {
		if _, ok := shapeFields[str(t)]; !ok {
			return nil, fieldError(path+".t", "use circle, rect, line, tri, arc, text or btn")
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
			return nil, fieldError(path+"."+key, "expected a number")
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
				return nil, fieldError(path+"."+key, "expected #RRGGBB")
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

func (h *wsHub) mcpScene(ctx context.Context, id, method string, args object, chime string, pcm []byte) (object, error) {
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
		if len(pcm) != 0 {
			h.playPCM(d, pcm)
		} else if chime != "" {
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
		view, _ := d.sceneState()
		targets := newSceneTargets(view, d.states)
		targets.collectPatch(patch)
		if err := targets.checkPatch(patch); err != nil {
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
	voice := b.voiceFor(who)
	pcm := b.clips.peek(voice, clean)
	if pcm == nil {
		var err error
		pcm, err = b.worker.synthesize(ctx, voice, clean)
		if err != nil {
			return nil, problem(502, "TTS synthesis failed")
		}
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
		h.wake(d, time.Now())
		h.playPCM(d, pcm)
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

func (b *bridge) mcpChime(ctx context.Context, who peer, kind string, pcm []byte) (object, error) {
	if len(pcm) == 0 && !validChime(kind) {
		return nil, fieldError("kind", "use tap, think, intercom or celebrate, or supply notes")
	}
	h := b.ws
	h.mu.Lock()
	if d := h.devices[strings.ToLower(who.DeviceID)]; d != nil && h.active(d) {
		if len(pcm) > 0 {
			h.playPCM(d, pcm)
		} else {
			h.chime(d, kind)
		}
		h.mu.Unlock()
	} else {
		h.mu.Unlock()
		if len(pcm) == 0 {
			pcm = chimePCM(kind)
		}
		if who.Port == 0 || !b.postPeer(ctx, who, "/speak", pcmType, pcm) {
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
