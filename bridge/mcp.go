package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	exitNode    = "echoear-exit"

	mcpInstructions = "Use the exact EchoEar device ID. Inspect live context, run an SVG drawing or sandboxed JS app, act on a device, or store persistent JSON. Only final assistant prose is spoken."
)

func mcpSchemaHash() string {
	return hashMCPSchema(mcpTools, mcpInstructions)
}

func hashMCPSchema(tools []object, instructions string) string {
	// json.Marshal sorts map keys, including nested schema properties.
	raw, err := json.Marshal(object{"tools": tools, "instructions": instructions})
	if err != nil {
		panic(err) // The built-in tool schemas must be JSON-serializable.
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

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
		if name == "" || !ok || !slices.ContainsFunc(mcpTools, func(tool object) bool { return tool["name"] == name }) {
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
		tool("inspect", "Read the live cat and its editable face_svg, clock, peers, screen, app, timers, memory keys, and saved cats/apps. Omit device_id for discovery.",
			schema(object{"device_id": text})),
		tool("run_app", "Display a 360×360 SVG, run a sandboxed JS app, load/save by slug, or exit back to the cat face. One shared app state across devices, render(state, player) per device.",
			schema(object{"devices": object{"anyOf": []object{text, array(text)}}, "title": text, "svg": text, "code": object{"type": "string", "maxLength": 16384},
				"load": text, "save_as": text, "tick_ms": number, "max_ticks": number, "exit": flag}, "devices")),
		tool("act", "Send a spoken intercom, play a melody/chime, change device controls, set/cancel a timer, or design/load a cat.",
			schema(object{"to": text, "intercom": schema(object{"from": text, "text": text}, "from", "text"), "notes": notes, "chime": enum("tap", "think", "intercom", "celebrate"),
				"volume": object{"type": "integer", "minimum": 0, "maximum": 100, "description": "Set manual volume"}, "auto_volume": flag, "sleep": flag,
				"timer": schema(object{"op": enum("set", "cancel"), "id": text, "all": flag, "in_seconds": number, "at": text, "label": text, "say": text, "notes": notes, "wake_agent": flag}, "op"),
				"cat": schema(object{"op": enum("update", "load", "reset_blank", "delete_saved"), "name": text, "face_bg": text, "mark_color": text, "accent_color": text,
					"personality": text, "voice": text, "face_svg": object{"type": "string", "maxLength": 16384, "description": "Complete SVG face with idle, blink, talk, listen, think and sleep pose layers"}, "slug": text, "configured": flag}, "op")}, "to")),
		tool("store", "Read/write persistent JSON in a device, shared, game:<name>, or app:<name> scope. expected_version is atomic; zero means no key exists.",
			schema(object{"op": enum("get", "set", "delete", "list"), "scope": text, "key": text, "value": object{}, "expected_version": number}, "op", "scope")),
	}
}()

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
	entry["battery_pct"], entry["volume"], entry["scene_rev"] = nil, nil, uint64(0)
	entry["sleeping"] = false
	h.mu.Lock()
	if d := h.devices[key]; d != nil && h.active(d) {
		entry["ws_connected"], entry["ws"], entry["online"], entry["mode"] = true, true, true, d.pose.mood
		entry["character"], entry["cat"] = d.cat.Name, d.cat.catLook
		entry["volume"], entry["scene_rev"], entry["firmware"] = d.vol, d.rev, d.firmware
		entry["auto_volume"], entry["sleeping"] = d.autoVol, d.asleep
		if d.board != "" {
			entry["board"], entry["shape"], entry["width"], entry["height"] = d.board, d.screen.shape, d.screen.width, d.screen.height
		}
		if d.pct >= 0 {
			entry["battery_pct"] = d.pct
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
			p["auto_volume"], p["sleeping"], p["voice"] = entry["auto_volume"], entry["sleeping"], entry["voice"]
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
		cat, me := b.catFor(who), out["me"].(object)
		me["cat"], me["configured"], me["personality"], me["face_svg"] = cat.catLook, cat.Configured, cat.Personality, cat.FaceSVG
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
	_, volumeSet := args["volume"]
	_, autoSet := args["auto_volume"]
	_, sleepSet := args["sleep"]
	if volumeSet || autoSet || sleepSet {
		if b.ws == nil {
			return nil, problem(503, "Device hub unavailable")
		}
		r, err := b.ws.mcpControl(ctx, to, args)
		if err != nil {
			return nil, err
		}
		out["volume"], out["auto_volume"], out["sleeping"] = r["volume"], r["auto_volume"], r["sleeping"]
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
		if err := b.playChime(who.DeviceID, kind, pcm); err != nil {
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
		if auto == (volume != nil) {
			return false, 0, false, problem(400, "Use auto without volume, or provide a manual volume")
		}
	}
	if auto {
		return true, initialAutoVolume, true, nil
	}
	if volume != nil {
		return false, int(*volume), true, nil
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
	return kind == "tap" || kind == "think" || kind == "intercom" || kind == "celebrate"
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
	d := h.devices[strings.ToLower(id)]
	if d == nil || !h.active(d) {
		return problem(409, "Device is not connected")
	}
	if len(pcm) > 0 {
		h.playPCM(d, pcm)
	} else {
		h.chime(d, kind)
	}
	return nil
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
