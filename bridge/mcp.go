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
	exitNode    = "echoear-exit"

	mcpInstructions = "Use exact EchoEar device IDs. Read keyed snapshots with state, change devices/cats/memory/timers/saved apps with patch, run or update screens with app, and send live audio with emit. Only final assistant prose is spoken."
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
			"instructions": mcpInstructions}
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
		if !ok || mcpInputSchema(name) == nil {
			sendJSON(w, 200, rpcError(req.ID, -32602, "Unknown tool or invalid arguments"))
			return
		}
		out, err := b.callMCP(r.Context(), name, args)
		if err != nil {
			out = object{"error": err.Error()}
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

var basicChimes = []string{"tap", "think", "intercom", "celebrate"}

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
	chimes := append(slices.Clone(basicChimes), slices.Sorted(maps.Keys(sfxPresets))...)
	memory := schema(object{"op": enum("set", "delete"), "scope": text, "key": text, "value": object{},
		"expected_version": object{"type": "integer", "minimum": 0, "description": "Require this version; 0 means absent"}}, "scope", "key")
	rename := schema(object{"from": text, "to": text, "title": text}, "from")
	rename["anyOf"] = []object{{"required": []string{"to"}}, {"required": []string{"title"}}}
	jsonObject := object{"type": "object", "additionalProperties": true}
	return []object{
		tool("state", "Read keyed device, cat, saved app, timer, and JSON memory snapshots. Use app_name to read one saved app with full code, SVG, and persistent state. With device_id, always includes me.device, me.cat, and me.active_app. Use include for specific slices or recent messages and diagnostics.",
			schema(object{"device_id": text, "include": array(enum(stateSlices...)), "memory_scopes": array(text),
				"app_name": text, "include_code": flag, "messages_limit": object{"type": "integer", "minimum": 1, "maximum": 30, "default": 10}})),
		tool("patch", "Change device controls, cat appearance, persistent JSON memory, a timer, or saved apps. Memory and saved app batches each commit atomically. Saved app deletes run before renames; missing deletes are ignored. Cat edits default to update; timer creation accepts in_seconds or at.",
			schema(object{"device_id": text,
				"device": schema(object{"volume": object{"type": "integer", "minimum": 0, "maximum": 100}, "auto_volume": flag, "sleep": flag, "voice": text}),
				"cat": schema(object{"op": enum("update", "load", "reset_blank", "delete_saved"), "name": text, "face_bg": text, "mark_color": text, "accent_color": text,
					"personality": text, "voice": text, "face_svg": object{"type": "string", "maxLength": 16384, "description": "Complete SVG face with idle, blink, talk, listen, think and sleep pose layers"},
					"slug": object{"type": "string", "description": "Saved cat slug for load/delete_saved"}, "configured": flag,
					"save_profile": object{"type": "boolean", "description": "Save an updated cat as a reusable profile"}}),
				"memory": object{"anyOf": []object{memory, array(memory)}},
				"apps":   schema(object{"delete": array(text), "rename": array(rename)}),
				"timer":  schema(object{"op": enum("set", "cancel"), "device_id": text, "id": text, "all": flag, "in_seconds": number, "at": text, "label": text, "say": text, "chime": enum(basicChimes...), "notes": notes, "wake_agent": flag, "app": text})})),
		tool("app", "Display SVG, launch/load/save/delete an app or drawing, exit to the cat, or shallow-patch one running JS app without restarting. One shared state renders separately for each player.",
			schema(object{"devices": object{"anyOf": []object{text, array(text)}}, "title": text, "svg": text, "code": object{"type": "string", "maxLength": 16384},
				"load": text, "save_as": text, "delete_saved": text, "initial_state": jsonObject, "state_patch": jsonObject, "tick_ms": number, "max_ticks": number, "exit": flag})),
		tool("emit", "Start a live two-way call, send a spoken intercom and card, play a melody, or play a sound effect. Use exact device IDs; calls cannot target all.",
			schema(object{"to": text, "intercom": schema(object{"from": text, "text": text}, "from", "text"),
				"call":  schema(object{"from": text}, "from"),
				"notes": notes, "sfx": enum(chimes...), "chime": enum(chimes...)}, "to")),
	}
}()

func mcpInputSchema(name string) object {
	for _, tool := range mcpTools {
		if tool["name"] == name {
			return tool["inputSchema"].(object)
		}
	}
	return nil
}

// Check closed objects against the published schema before dispatch. Leave
// value constraints and operation-specific fields to the runtime decoders.
func checkMCPFields(value any, schema object, path string) error {
	if branches, ok := schema["anyOf"].([]object); ok {
		for _, branch := range branches {
			if err := checkMCPFields(value, branch, path); err != nil {
				return err
			}
		}
	}
	switch v := value.(type) {
	case object:
		if schema["additionalProperties"] == false {
			props := schema["properties"].(object)
			for _, key := range slices.Sorted(maps.Keys(v)) {
				child, ok := props[key].(object)
				if !ok {
					return fieldError(path+key, "unknown argument")
				}
				if err := checkMCPFields(v[key], child, path+key+"."); err != nil {
					return err
				}
			}
		}
	case []any:
		if item, ok := schema["items"].(object); ok {
			for _, value := range v {
				if err := checkMCPFields(value, item, path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *bridge) lookupDevice(id string) (peer, error) {
	p, _, err := b.findDevice(id)
	if err == nil {
		p.Character = b.catFor(p).Name
	}
	return p, err
}

func (h *wsHub) deviceStatus(entry object, id string, full bool) {
	key := strings.ToLower(id)
	entry["ws_connected"], entry["ws"], entry["mode"] = false, false, "offline"
	entry["battery_pct"], entry["volume"], entry["scene_rev"] = nil, nil, uint64(0)
	entry["sleeping"] = false
	h.mu.Lock()
	if d := h.devices[key]; d != nil && h.active(d) {
		entry["ws_connected"], entry["ws"], entry["online"], entry["mode"] = true, true, true, d.pose.mood
		entry["character"], entry["cat"], entry["voice"] = d.cat.Name, d.cat.catLook, d.voice
		entry["volume"], entry["scene_rev"], entry["firmware"] = d.vol, d.rev, d.firmware
		entry["caps"] = d.caps
		entry["auto_volume"], entry["sleeping"] = d.autoVol, d.asleep
		if d.board != "" {
			entry["board"], entry["shape"], entry["width"], entry["height"] = d.board, d.screen.shape, d.screen.width, d.screen.height
		}
		if d.caps.Battery && d.pct >= 0 {
			entry["battery_pct"] = d.pct
		}
		if full {
			cat := d.cat
			cat.Voice = d.voice
			entry["cat"], entry["voice"] = cat, d.voice
			title := d.scene.title
			if title == "" {
				title = "Cat"
			}
			entry["screen"] = object{"title": title, "shape": d.screen.shape, "rev": d.rev, "hit_ids": appHitIDs(d.svg)}
			entry["width"], entry["height"] = d.screen.width, d.screen.height
			entry["wifi_rssi"], entry["cpu_pct"] = d.telemetry.WiFi, d.telemetry.CPU
			if d.telemetry.WiFi == nil {
				entry["wifi_rssi"] = d.telemetry.RSSI
			}
			if a := d.app; a != nil {
				entry["active_app"] = h.activeAppState(a)
				if a.snapshot != nil {
					entry["active_app_state"] = a.snapshot
				}
			}
		}
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	auto, level := h.b.volumePrefs(id)
	entry["auto_volume"] = auto
	if !auto {
		entry["volume"] = level
	}
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
	if err := checkMCPFields(args, mcpInputSchema(name), ""); err != nil {
		return nil, err
	}
	switch name {
	case "state":
		return b.mcpState(ctx, args)
	case "patch":
		return b.mcpPatch(ctx, args)
	case "app":
		return b.runApp(ctx, args)
	case "emit":
		return b.mcpEmit(ctx, args)
	default:
		return nil, problem(400, "Unknown tool")
	}
}

func (b *bridge) mcpEmit(ctx context.Context, args object) (object, error) {
	to := str(args["to"])
	if !deviceIDRE.MatchString(to) {
		return nil, problem(400, "Expected exact device ID or all")
	}
	out := object{"ok": true, "to": to}
	if raw, ok := args["call"]; ok {
		call, _ := raw.(object)
		from := str(call["from"])
		if len(args) != 2 || broadcast(to) || !deviceIDRE.MatchString(from) {
			return nil, fieldError("call", "use only call.from and to with exact device IDs")
		}
		if b.ws == nil {
			return nil, problem(503, "Device hub unavailable")
		}
		result, err := b.ws.mcpCall(from, to)
		if err != nil {
			return nil, err
		}
		out["call"] = result
		return out, nil
	}
	var pcm []byte
	var kind string
	var audioField string
	for _, key := range []string{"notes", "sfx", "chime"} {
		raw, ok := args[key]
		if !ok {
			continue
		}
		if audioField != "" {
			return nil, problem(400, "Use only one of notes, sfx, or chime")
		}
		audioField = key
		if key == "notes" {
			var err error
			_, pcm, err = melodyInput(raw, "notes")
			if err != nil {
				return nil, err
			}
		} else if kind = str(raw); !validChime(kind) {
			var err error
			pcm, err = sfxPCM(kind)
			if err != nil {
				return nil, fieldError(key, "unknown kind")
			}
		}
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
	if audioField != "" {
		if err := b.playChime(to, kind, pcm); err != nil {
			return nil, err
		}
		out["played"] = true
	}
	if len(out) == 2 {
		return nil, problem(400, "Expected an action")
	}
	return out, nil
}

func volumeCommand(args object, key string) (auto bool, level int, set bool, err error) {
	volume, err := mcpUint(args, "volume")
	if err != nil || volume != nil && *volume > 100 {
		return false, 0, false, problem(400, "Volume must be an integer from 0 to 100")
	}
	if _, ok := args[key]; ok {
		auto, err = catBool(args, key, false)
		if err != nil {
			return false, 0, false, err
		}
		if auto && volume != nil {
			return false, 0, false, problem(400, "Use auto without manual volume")
		}
	}
	if auto {
		return true, initialAutoVolume, true, nil
	}
	if volume != nil {
		return false, int(*volume), true, nil
	}
	if _, present := args[key]; present {
		return false, -1, true, nil // Hold the current level when disabling auto-volume.
	}
	return false, 0, false, nil
}

func (h *wsHub) mcpControl(ctx context.Context, id string, args object) (object, error) {
	auto, level, setVolume, err := volumeCommand(args, "auto_volume")
	if err != nil {
		return nil, err
	}
	sleep, setSleep := args["sleep"]
	asleep, isBool := sleep.(bool)
	if setSleep && !isBool {
		return nil, problem(400, "Sleep must be a boolean")
	}
	if !setVolume && !setSleep {
		return nil, problem(400, "Expected volume, auto_volume, or sleep")
	}
	if !setSleep {
		who, err := h.b.lookupDevice(id)
		if err != nil {
			return nil, err
		}
		id = who.DeviceID
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) {
		if setSleep {
			return nil, problem(409, "Device is not connected")
		}
		h.mu.Unlock()
		h.settingsMu.Lock()
		if level < 0 {
			_, level = h.b.volumePrefs(id)
		}
		err := h.b.saveVolumePrefs(ctx, id, auto, level)
		h.settingsMu.Unlock()
		h.mu.Lock()
		if err != nil {
			return nil, err
		}
		return object{"ok": true, "device_id": id, "volume": level, "auto_volume": auto, "sleeping": false}, nil
	}
	if setVolume {
		if err := h.setVolume(ctx, d, auto, level, ""); err != nil {
			return nil, err
		}
	}
	if setSleep {
		now := time.Now()
		d.last = now
		h.setSleeping(d, asleep)
		h.render(d, now, false)
	}
	return object{"ok": true, "device_id": d.who.DeviceID, "volume": d.vol, "auto_volume": d.autoVol, "sleeping": d.asleep}, nil
}

func validChime(kind string) bool {
	return slices.Contains(basicChimes, kind)
}

func (h *wsHub) chime(d *wsDevice, kind string) {
	if kind == "celebrate" {
		kind = "intercom" // Existing firmware's happy tone.
	}
	h.push(d, object{"op": "chime", "kind": kind}, false)
}

func (b *bridge) playChime(id, kind string, pcm []byte) error {
	if b.ws == nil {
		return problem(503, "Device hub unavailable")
	}
	h := b.ws
	h.mu.Lock()
	defer h.mu.Unlock()
	if id == "all" {
		played := false
		for _, d := range h.devices {
			if h.active(d) {
				h.playSignal(d, kind, pcm)
				played = true
			}
		}
		if !played {
			return problem(409, "No devices connected")
		}
		return nil
	}
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) || d.who.DeviceID != id {
		return problem(409, "Device is not connected")
	}
	h.playSignal(d, kind, pcm)
	return nil
}

func (h *wsHub) playSignal(d *wsDevice, kind string, pcm []byte) {
	if len(pcm) > 0 {
		h.playPCM(d, pcm)
	} else {
		h.chime(d, kind)
	}
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
	var recipients []peer
	if to == "all" {
		recipients = b.intercomTargets(others, to)
	} else {
		recipients = slices.DeleteFunc(others, func(p peer) bool { return p.DeviceID != to })
	}
	if len(recipients) == 0 {
		return nil, problem(404, "Recipient not found")
	}
	out := b.deliver(ctx, sender, recipients, text, nil)
	if len(out.sent) == 0 {
		return nil, problem(409, "No recipients connected")
	}
	return object{"ok": true, "sent_to": out.sent, "delivered_to": out.delivered, "failed_to": out.failed}, nil
}

func fieldError(path, format string, args ...any) error {
	return problem(400, path+": "+fmt.Sprintf(format, args...))
}
