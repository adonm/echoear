package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/dop251/goja"
)

type savedApp struct {
	Name, Title, Kind, SVG, Code, Author string
	TickMS, MaxTicks                     int
}

func (b *bridge) loadApp(ctx context.Context, name string) (savedApp, error) {
	a := savedApp{Name: name}
	if b.db == nil {
		return a, problem(503, "Apps unavailable")
	}
	if !slugRE.MatchString(name) {
		return a, problem(400, "Invalid app name")
	}
	err := b.db.QueryRowContext(ctx, `SELECT title,kind,svg,code,tick_ms,max_ticks,author FROM apps WHERE name=?`, name).
		Scan(&a.Title, &a.Kind, &a.SVG, &a.Code, &a.TickMS, &a.MaxTicks, &a.Author)
	if errors.Is(err, sql.ErrNoRows) {
		err = problem(404, "App not found")
	}
	return a, err
}

func (b *bridge) saveApp(ctx context.Context, a savedApp) error {
	if b.db == nil {
		return problem(503, "Apps unavailable")
	}
	result, err := b.db.ExecContext(ctx, `INSERT INTO apps (name,title,kind,svg,code,tick_ms,max_ticks,author,updated_at)
		SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM apps WHERE name=?) OR (SELECT count(*) FROM apps) < 128
		ON CONFLICT(name) DO UPDATE SET title=excluded.title,kind=excluded.kind,svg=excluded.svg,code=excluded.code,
		tick_ms=excluded.tick_ms,max_ticks=excluded.max_ticks,author=excluded.author,updated_at=excluded.updated_at`,
		a.Name, a.Title, a.Kind, a.SVG, a.Code, a.TickMS, a.MaxTicks, a.Author, time.Now().UnixMilli(), a.Name)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return problem(409, "Saved app library is full")
	}
	return err
}

func (b *bridge) listApps(ctx context.Context, includeCode bool) ([]object, error) {
	out := []object{}
	if b.db == nil {
		return out, nil
	}
	columns := "name,title,kind,author,updated_at,tick_ms,max_ticks"
	if includeCode {
		columns += ",svg,code"
	}
	rows, err := b.db.QueryContext(ctx, "SELECT "+columns+" FROM apps ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a savedApp
		var updated int64
		fields := []any{&a.Name, &a.Title, &a.Kind, &a.Author, &updated, &a.TickMS, &a.MaxTicks}
		if includeCode {
			fields = append(fields, &a.SVG, &a.Code)
		}
		if err := rows.Scan(fields...); err != nil {
			return nil, err
		}
		entry := object{"name": a.Name, "title": a.Title, "kind": a.Kind, "author": a.Author, "updated_at": updated, "tick_ms": a.TickMS, "max_ticks": a.MaxTicks}
		if a.SVG != "" {
			entry["svg"] = a.SVG
		}
		if a.Code != "" {
			entry["code"] = a.Code
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

func (b *bridge) appState(ctx context.Context, includeCode bool) (object, error) {
	list, err := b.listApps(ctx, includeCode)
	if err != nil {
		return nil, err
	}
	out := object{}
	for _, a := range list {
		delete(a, "author")
		out[str(a["name"])] = a
	}
	return out, nil
}

func (b *bridge) deleteSavedApp(ctx context.Context, name string) (bool, error) {
	if !slugRE.MatchString(name) {
		return false, problem(400, "Invalid app name")
	}
	if b.db == nil {
		return false, problem(503, "Apps unavailable")
	}
	deleted, err := deleteRow(ctx, b.db, "DELETE FROM apps WHERE name=?", name)
	if deleted {
		b.appsChanged()
	}
	return deleted, err
}

func (b *bridge) appAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/apps" && r.Method == http.MethodGet:
		list, err := b.listApps(ctx, false)
		return object{"apps": list}, err
	case path == "/api/apps/run" && r.Method == http.MethodPost:
		data, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		return b.runSavedApp(ctx, str(data["device_id"]), str(data["name"]))
	case r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/apps/")
		_, err := b.deleteSavedApp(ctx, name)
		if err != nil {
			return nil, err
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}

type appPlayer struct {
	id, name string
	index    int
}

type appEffect struct {
	kind, text string
	value      json.RawMessage
	pcm        []byte
	all        bool
}

// One worker owns the VM and lastScreens after initialization. The hub owns
// device membership, queued events, and tick scheduling.
type appInstance struct {
	spec                                savedApp
	players                             []appPlayer
	devices                             []*wsDevice
	vm                                  *goja.Runtime
	self, state                         goja.Value
	render, parse, stringify, errorText goja.Callable
	merge                               goja.Callable
	hooks                               map[string]goja.Callable
	scope                               string
	memory                              map[string]json.RawMessage
	effects                             []appEffect
	allowEffects                        bool
	geometry                            []screenGeometry
	buildScreen                         func(string, bool, screenGeometry) (svgScreen, error)
	lastScreens                         []svgScreen
	ctx                                 context.Context
	cancel                              context.CancelFunc
	events                              []appEvent
	running, tickQueued                 bool
	nextTick, timeOfSpeech              time.Time
	remaining                           int
	snapshot                            json.RawMessage // Immutable JSON published under the hub lock.
}

type appEvent struct {
	i, x, y  int
	kind, id string
	now      time.Time
	patch    json.RawMessage
	done     chan appPatchResult
}

type appPatchResult struct {
	out object
	err error
}

func (a *appInstance) runJS(fn func() (goja.Value, error)) (goja.Value, error) {
	vm := a.vm
	done := make(chan struct{})
	timer := time.AfterFunc(50*time.Millisecond, func() {
		vm.Interrupt("timeout")
		close(done)
	})
	defer func() {
		if !timer.Stop() {
			<-done
		}
		vm.ClearInterrupt()
	}()
	v, err := fn()
	if ex, ok := err.(*goja.Exception); ok {
		var out bytes.Buffer
		if msg, textErr := a.errorText(goja.Undefined(), ex.Value()); textErr == nil {
			if s, ok := msg.(goja.String); ok {
				out.WriteString(s.Substring(0, min(512, s.Length())).String())
			}
		}
		if out.Len() == 0 {
			out.WriteString("JS exception")
		}
		for _, frame := range ex.Stack()[:min(8, len(ex.Stack()))] {
			out.WriteString("\n\tat ")
			frame.Write(&out)
		}
		return v, errors.New(out.String())
	}
	if trace, ok := err.(interface{ String() string }); ok {
		return v, errors.New(strings.TrimSpace(trace.String()))
	}
	return v, err
}

func (a *appInstance) call(fn goja.Callable, args ...goja.Value) (goja.Value, error) {
	return a.runJS(func() (goja.Value, error) { return fn(a.self, args...) })
}

func (a *appInstance) player(i int) goja.Value {
	p := a.players[i]
	// Copy primitives into a native JS object, not a reflected Go map.
	v := a.vm.NewObject()
	for k, value := range map[string]any{"id": p.id, "name": p.name, "index": p.index} {
		_ = v.DefineDataProperty(k, a.vm.ToValue(value), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	}
	return v
}

func initApp(spec savedApp, players []appPlayer, memory map[string]json.RawMessage, devices []*wsDevice, initial json.RawMessage, renderer func(string, bool, screenGeometry) (svgScreen, error)) (*appInstance, []svgScreen, error) {
	a := &appInstance{spec: spec, players: players, memory: memory, devices: devices, scope: "app:" + spec.Name,
		geometry: make([]screenGeometry, len(players)), buildScreen: renderer, lastScreens: make([]svgScreen, len(players))}
	for i := range players {
		// Geometry and the client role do not change after hello.
		if devices == nil {
			a.geometry[i] = roundScreen
		} else if devices[i].client.role != "web" {
			a.geometry[i] = devices[i].screen
		}
	}
	if spec.Code != "" {
		if len(spec.Code) > 16<<10 {
			return nil, nil, problem(400, "App code exceeds 16 KB")
		}
		if err := a.initVM(initial); err != nil {
			return nil, nil, err
		}
	}
	screens, err := a.screens(nil)
	if err == nil {
		a.snapshot, err = a.stateJSON()
	}
	return a, screens, err
}

func (a *appInstance) initVM(initial json.RawMessage) error {
	vm := goja.New()
	a.vm = vm
	vm.SetMaxCallStackSize(512)
	a.errorText, _ = goja.AssertFunction(vm.Get("String"))
	hostJSON := vm.Get("JSON").ToObject(vm)
	a.parse, _ = goja.AssertFunction(hostJSON.Get("parse"))
	a.stringify, _ = goja.AssertFunction(hostJSON.Get("stringify"))
	merge, err := vm.RunString(`(function (state, patch) {
		if (state === null || typeof state !== "object" || Array.isArray(state)) {
			throw new TypeError("state_patch requires an object app state");
		}
		return {...state, ...patch};
	})`)
	if err != nil {
		return err
	}
	a.merge, _ = goja.AssertFunction(merge)
	bad := func(err error) { panic(vm.NewTypeError("%s", err.Error())) }
	asString := func(v goja.Value) (string, bool) {
		s, ok := v.(goja.String)
		if !ok || s.Length() > 16384 {
			return "", false
		}
		return s.String(), true
	}
	run := func(name string, f func(goja.FunctionCall) goja.Value) {
		_ = vm.Set(name, func(c goja.FunctionCall) goja.Value {
			if !a.allowEffects {
				panic(vm.NewTypeError("%s is only available in onTap/onDrag/onUp/onTick", name))
			}
			if len(a.effects) >= 8 {
				panic(vm.NewTypeError("Too many app effects"))
			}
			return f(c)
		})
	}
	for _, name := range []string{"play", "playAll"} {
		run(name, func(c goja.FunctionCall) goja.Value {
			var notes any
			v := c.Argument(0)
			if s, ok := asString(v); ok {
				notes = s
			} else if obj, ok := v.(*goja.Object); ok && obj.ClassName() == "Array" && obj.Get("length").ToInteger() <= 32 {
				list := []any{}
				for i := range int(obj.Get("length").ToInteger()) {
					s, ok := asString(obj.Get(fmt.Sprint(i)))
					if !ok {
						panic(vm.NewTypeError("Expected note strings"))
					}
					list = append(list, s)
				}
				notes = list
			}
			_, pcm, err := melodyInput(notes, "notes")
			if err != nil {
				bad(err)
			}
			a.effects = append(a.effects, appEffect{kind: "pcm", pcm: pcm, all: name == "playAll"})
			return goja.Undefined()
		})
	}
	for _, name := range []string{"sfx", "sfxAll"} {
		run(name, func(c goja.FunctionCall) goja.Value {
			kind, _ := asString(c.Argument(0))
			pcm, err := sfxPCM(kind)
			if err != nil {
				bad(err)
			}
			a.effects = append(a.effects, appEffect{kind: "pcm", pcm: pcm, all: name == "sfxAll"})
			return goja.Undefined()
		})
	}
	run("chime", func(c goja.FunctionCall) goja.Value {
		kind, _ := asString(c.Argument(0))
		if !validChime(kind) {
			panic(vm.NewTypeError("Unknown chime kind"))
		}
		a.effects = append(a.effects, appEffect{kind: "chime", text: kind})
		return goja.Undefined()
	})
	run("say", func(c goja.FunctionCall) goja.Value {
		text, _ := asString(c.Argument(0))
		if clean := shortText(text, 100, true); clean != "" {
			a.effects = append(a.effects, appEffect{kind: "say", text: clean})
		}
		return goja.Undefined()
	})
	run("exit", func(goja.FunctionCall) goja.Value {
		a.effects = append(a.effects, appEffect{kind: "exit"})
		return goja.Undefined()
	})
	_ = vm.Set("storeGet", func(c goja.FunctionCall) goja.Value {
		key, _ := asString(c.Argument(0))
		if !memoryKeyRE.MatchString(key) {
			panic(vm.NewTypeError("Invalid app memory key"))
		}
		value := a.memory[key]
		for _, e := range slices.Backward(a.effects) {
			if e.kind == "store" && e.text == key {
				value = e.value
				break
			}
		}
		if len(value) == 0 {
			return goja.Undefined()
		}
		v, err := a.parse(goja.Undefined(), vm.ToValue(string(value)))
		if err != nil {
			bad(err)
		}
		return v
	})
	run("storeSet", func(c goja.FunctionCall) goja.Value {
		key, _ := asString(c.Argument(0))
		if !memoryKeyRE.MatchString(key) {
			panic(vm.NewTypeError("Invalid app memory key"))
		}
		raw, err := a.stringify(goja.Undefined(), c.Argument(1))
		if err != nil {
			bad(err)
		}
		s, ok := asString(raw)
		if !ok || len(s) > 16384 {
			panic(vm.NewTypeError("Expected a JSON value of up to 16384 bytes"))
		}
		a.effects = append(a.effects, appEffect{kind: "store", text: key, value: json.RawMessage(s)})
		return goja.Undefined()
	})
	_ = vm.Set("app", goja.Undefined())
	if _, err := a.runJS(func() (goja.Value, error) { return vm.RunScript("app.js", a.spec.Code) }); err != nil {
		return err
	}
	methods, err := a.runJS(func() (goja.Value, error) {
		return vm.RunScript("app-contract.js", `(function() {
			if (app && typeof app === "object") return [app, app.init, app.render, app.onTap, app.onDrag, app.onUp, app.onTick];
			return [void 0, typeof init === "function" ? init : void 0, typeof render === "function" ? render : void 0,
				typeof onTap === "function" ? onTap : void 0, typeof onDrag === "function" ? onDrag : void 0,
				typeof onUp === "function" ? onUp : void 0, typeof onTick === "function" ? onTick : void 0];
		})()`)
	})
	if err != nil {
		return err
	}
	obj := methods.(*goja.Object)
	a.self = obj.Get("0")
	init, _ := goja.AssertFunction(obj.Get("1"))
	a.render, _ = goja.AssertFunction(obj.Get("2"))
	a.hooks = make(map[string]goja.Callable, 4)
	for i, kind := range []string{"tap", "drag", "up", "tick"} {
		a.hooks[kind], _ = goja.AssertFunction(obj.Get(fmt.Sprint(i + 3)))
	}
	if a.render == nil {
		return fmt.Errorf("app.js must define render(state, player)")
	}
	ctx := vm.NewObject()
	players := make([]any, len(a.players))
	for i := range players {
		players[i] = a.player(i)
	}
	_ = ctx.DefineDataProperty("players", vm.NewArray(players...), goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	a.state = vm.NewObject()
	if initial != nil {
		a.state, err = a.call(a.parse, vm.ToValue(string(initial)))
		if err != nil {
			return err
		}
	}
	_ = ctx.DefineDataProperty("initial_state", a.state, goja.FLAG_FALSE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	_ = ctx.DefineDataProperty("state", a.state, goja.FLAG_TRUE, goja.FLAG_FALSE, goja.FLAG_TRUE)
	if init != nil {
		state, err := a.call(init, ctx)
		if err != nil {
			return err
		}
		if !goja.IsUndefined(state) {
			a.state = state
		}
	}
	return nil
}

func (a *appInstance) stateJSON() (json.RawMessage, error) {
	if a.vm == nil {
		return nil, nil
	}
	v, err := a.call(a.stringify, a.state)
	if err != nil {
		return nil, fmt.Errorf("app state must be JSON: %w", err)
	}
	if s, ok := v.(goja.String); ok && s.Length() <= 64<<10 {
		if raw := s.String(); len(raw) <= 64<<10 {
			return json.RawMessage(raw), nil
		}
	}
	return nil, problem(400, "App state must fit in 64 KB of JSON")
}

func (a *appInstance) applyStatePatch(raw json.RawMessage, connected []bool) ([]svgScreen, json.RawMessage, error) {
	patch, err := a.call(a.parse, a.vm.ToValue(string(raw)))
	if err != nil {
		return nil, nil, err
	}
	next, err := a.call(a.merge, a.state, patch)
	if err != nil {
		return nil, nil, err
	}
	before := a.state
	a.state = next
	views, err := a.screens(connected)
	var snapshot json.RawMessage
	if err == nil {
		snapshot, err = a.stateJSON()
	}
	if err != nil {
		a.state = before
	}
	return views, snapshot, err
}

func (a *appInstance) screens(connected []bool) ([]svgScreen, error) {
	out := make([]svgScreen, len(a.players))
	for i := range out {
		if connected != nil && !connected[i] {
			continue
		}
		raw := a.spec.SVG
		if a.vm != nil {
			value, err := a.call(a.render, a.state, a.player(i))
			if err != nil {
				return nil, err
			}
			s, ok := value.(goja.String)
			if !ok {
				return nil, fmt.Errorf("app.js render must return an SVG string")
			}
			if s.Length() > maxSVG {
				return nil, fmt.Errorf("app.js render: SVG exceeds 64 KB")
			}
			raw = s.String()
		}
		if last := a.lastScreens[i]; last.source != "" && last.raw == raw {
			out[i] = last
			continue
		}
		screen, err := a.buildScreen(raw, true, a.geometry[i])
		if err != nil {
			return nil, fmt.Errorf("render for player %d: %w", i, err)
		}
		a.lastScreens[i], out[i] = screen, screen
	}
	return out, nil
}

func (a *appInstance) update(ev appEvent, connected []bool) ([]svgScreen, error) {
	fn := a.hooks[ev.kind]
	if fn == nil {
		return nil, nil
	}
	args := []goja.Value{a.state}
	if ev.i >= 0 {
		args = append(args, a.player(ev.i), a.vm.ToValue(ev.id), a.vm.ToValue(ev.x), a.vm.ToValue(ev.y))
	}
	a.effects = nil
	a.allowEffects = true
	state, err := a.call(fn, args...)
	a.allowEffects = false
	if err != nil {
		return nil, err
	}
	if !goja.IsUndefined(state) {
		a.state = state
	}
	if slices.ContainsFunc(a.effects, func(e appEffect) bool { return e.kind == "exit" }) {
		return nil, nil
	}
	return a.screens(connected)
}

func appDevices(raw any) ([]string, error) {
	var ids []string
	switch v := raw.(type) {
	case string:
		ids = []string{v}
	case []any:
		for _, item := range v {
			ids = append(ids, str(item))
		}
	}
	if len(ids) == 0 || len(ids) > 8 {
		return nil, problem(400, "Expected 1–8 device IDs")
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !deviceIDRE.MatchString(id) || seen[strings.ToLower(id)] {
			return nil, problem(400, "Invalid or duplicate device ID")
		}
		seen[strings.ToLower(id)] = true
	}
	return ids, nil
}

func (b *bridge) runSavedApp(ctx context.Context, id, name string) (object, error) {
	return b.runApp(ctx, object{"devices": id, "load": name})
}

func (b *bridge) runApp(ctx context.Context, args object) (object, error) {
	if name, present := args["delete_saved"]; present {
		if len(args) != 1 {
			return nil, problem(400, "Use delete_saved without other app fields")
		}
		deleted, err := b.deleteSavedApp(ctx, str(name))
		return object{"ok": true, "delete_saved": name, "deleted": deleted, "devices": []string{}, "title": "", "hit_ids": []string{}}, err
	}
	ids, err := appDevices(args["devices"])
	if err != nil {
		return nil, err
	}
	h := b.ws
	if h == nil {
		return nil, problem(503, "Device hub unavailable")
	}
	if raw, present := args["state_patch"]; present {
		if len(args) != 2 {
			return nil, problem(400, "Use state_patch with devices only")
		}
		patch, err := appJSONObject(raw, "state_patch")
		if err != nil {
			return nil, err
		}
		return h.patchRunningApp(ctx, ids, patch)
	}
	if args["exit"] == true && len(args) != 2 {
		return nil, problem(400, "Use exit with devices only")
	}
	h.mu.Lock()
	players := make([]appPlayer, 0, len(ids))
	devices := make([]*wsDevice, 0, len(ids))
	gens := make([]uint64, 0, len(ids))
	for _, id := range ids {
		d := h.devices[strings.ToLower(id)]
		if d == nil || !h.active(d) {
			h.mu.Unlock()
			return nil, problem(409, "Device is not connected: "+id)
		}
		devices = append(devices, d)
		gens = append(gens, d.appGen)
		players = append(players, appPlayer{d.who.DeviceID, d.cat.Name, len(players)})
	}
	h.mu.Unlock()
	if args["exit"] == true {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, d := range devices {
			if h.active(d) && d.app != nil {
				h.closeApp(d.app, time.Now())
			}
		}
		return object{"ok": true, "devices": ids, "title": "", "hit_ids": []string{}}, nil
	}
	a := savedApp{SVG: str(args["svg"]), Code: str(args["code"]), Title: shortText(args["title"], 60, false), Author: players[0].name}
	if name, provided := args["load"]; provided {
		if a.SVG != "" || a.Code != "" {
			return nil, problem(400, "Use load or svg/code, not both")
		}
		a, err = b.loadApp(ctx, str(name))
		if err != nil {
			return nil, err
		}
		if title := shortText(args["title"], 60, false); title != "" {
			a.Title = title
		}
	}
	save := str(args["save_as"])
	if _, present := args["save_as"]; present {
		if !slugRE.MatchString(save) {
			return nil, problem(400, "Invalid save_as slug")
		}
		a.Name = save
	}
	if a.Title == "" {
		a.Title = "EchoEar App"
	}
	if a.Name == "" {
		a.Name = truncate(catSlug(a.Title), 48)
		if a.Name == "" {
			a.Name = "untitled"
		}
	}
	switch {
	case a.Code != "":
		a.Kind = "app"
	case a.SVG != "":
		a.Kind = "drawing"
	default:
		return nil, problem(400, "Expected svg, code, or load")
	}
	for _, o := range []struct {
		key       string
		dest      *int
		low, high int
	}{
		{"tick_ms", &a.TickMS, 250, 10000}, {"max_ticks", &a.MaxTicks, 1, 240},
	} {
		n, err := mcpUint(args, o.key)
		if err != nil {
			return nil, err
		}
		if n != nil {
			*o.dest = int(min(uint64(o.high), max(uint64(o.low), *n)))
		}
	}
	if a.TickMS != 0 && a.MaxTicks == 0 {
		a.MaxTicks = 240
	}
	if a.SVG != "" {
		clean, err := parseSVG(a.SVG)
		if err != nil {
			return nil, problem(400, err.Error())
		}
		a.SVG = svgString(clean)
		if a.Code != "" {
			var geometry screenGeometry
			if slices.ContainsFunc(devices, func(d *wsDevice) bool { return d.client.role == "device" }) {
				geometry = roundScreen
			}
			if _, err := b.buildScreen(a.SVG, false, geometry); err != nil {
				return nil, problem(400, err.Error())
			}
		}
	}
	memory := map[string]json.RawMessage{}
	var initial json.RawMessage
	if raw, present := args["initial_state"]; present {
		if a.Code == "" {
			return nil, problem(400, "initial_state requires a JS app")
		}
		initial, err = appJSONObject(raw, "initial_state")
		if err != nil {
			return nil, err
		}
	}
	if a.Code != "" {
		values, err := b.memoryEntries(ctx, "app:"+a.Name)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			memory[str(value["key"])] = value["value"].(json.RawMessage)
		}
	}
	inst, screens, err := initApp(a, players, memory, devices, initial, b.buildScreen)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	h.mu.Lock()
	for i, d := range devices {
		if !h.active(d) {
			h.mu.Unlock()
			return nil, problem(409, "Device disconnected: "+d.who.DeviceID)
		}
		if d.appGen != gens[i] {
			h.mu.Unlock()
			return nil, problem(409, "Device view changed: "+d.who.DeviceID)
		}
	}
	if save != "" {
		if err := b.saveApp(ctx, a); err != nil {
			h.mu.Unlock()
			return nil, err
		}
	}
	inst.ctx, inst.cancel = context.WithCancel(b.root)
	now := time.Now()
	if inst.hooks["tick"] != nil && a.TickMS != 0 {
		inst.remaining = a.MaxTicks
		inst.nextTick = now.Add(time.Duration(a.TickMS) * time.Millisecond)
	}
	out := object{"ok": true, "devices": ids, "title": a.Title, "hit_ids": appHitIDs(screens...)}
	if inst.snapshot != nil {
		out["state"] = inst.snapshot
	}
	for _, d := range devices {
		if d.app == nil {
			continue
		}
		old := d.app
		for _, other := range old.devices {
			if other != nil {
				h.clearView(other)
				if !slices.Contains(devices, other) {
					h.render(other, now, false)
				}
			}
		}
	}
	for i, d := range devices {
		h.clearView(d)
		d.app, d.appView, d.agentSetScene = inst, screens[i], d.busy
		h.wake(d, now)
		h.render(d, now, false)
	}
	h.mu.Unlock()
	if save != "" {
		b.appsChanged()
	}
	for _, p := range players {
		text := "Started: " + a.Title
		if a.Kind == "drawing" {
			text = "Drew: " + a.Title
		}
		entry := newChat("card", p.name, text, "app")
		entry.SceneTitle = a.Title
		b.chat(p.id, entry, true)
	}
	if save != "" {
		out["saved_as"] = save
	}
	return out, nil
}

func appJSONObject(raw any, field string) (json.RawMessage, error) {
	if _, ok := raw.(object); !ok {
		return nil, fieldError(field, "expected a JSON object")
	}
	data, err := json.Marshal(raw)
	if err != nil || len(data) > 64<<10 {
		return nil, fieldError(field, "expected at most 64 KB of JSON")
	}
	return data, nil
}

func appHitIDs(screens ...svgScreen) []string {
	seen, hits := map[string]bool{}, []string{}
	for _, screen := range screens {
		for _, hit := range screen.hits {
			if !seen[hit.id] {
				seen[hit.id] = true
				hits = append(hits, hit.id)
			}
		}
	}
	return hits
}

func (h *wsHub) activeAppState(a *appInstance) object {
	ids := []string{}
	for _, d := range a.devices {
		if d != nil && h.active(d) {
			ids = append(ids, d.who.DeviceID)
		}
	}
	out := object{"name": a.spec.Name, "title": a.spec.Title, "kind": a.spec.Kind, "devices": ids, "ticks_left": a.remaining}
	if a.snapshot != nil {
		out["state"] = a.snapshot
	}
	return out
}

func (h *wsHub) patchRunningApp(ctx context.Context, ids []string, patch json.RawMessage) (object, error) {
	h.mu.Lock()
	var a *appInstance
	for _, id := range ids {
		d := h.devices[strings.ToLower(id)]
		if d == nil || !h.active(d) || d.app == nil || d.app.vm == nil {
			h.mu.Unlock()
			return nil, problem(409, "No running JS app on "+id)
		}
		if a != nil && d.app != a {
			h.mu.Unlock()
			return nil, problem(409, "Devices must belong to the same running JS app")
		}
		a = d.app
	}
	done := make(chan appPatchResult, 1)
	ev := appEvent{kind: "patch", i: -1, now: time.Now(), patch: patch, done: done}
	ok := h.updateApp(a, ev)
	h.mu.Unlock()
	if !ok {
		return nil, problem(409, "App event queue is full")
	}
	select {
	case result := <-done:
		return result.out, result.err
	case <-a.ctx.Done():
		return nil, problem(409, "App closed before applying state_patch")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *bridge) appsChanged() {
	if b.ws == nil {
		return
	}
	b.ws.mu.Lock()
	defer b.ws.mu.Unlock()
	for c := range b.ws.clients {
		if c.role == "web" && c.scope == nil {
			c.enqueue(textFrame(object{"type": "saved_apps"}))
		}
	}
}

func (h *wsHub) dropApp(d *wsDevice) {
	if a := d.app; a != nil {
		for i, p := range a.devices {
			if p == d {
				a.devices[i] = nil
				break
			}
		}
		d.app, d.appView = nil, svgScreen{}
		if !slices.ContainsFunc(a.devices, func(d *wsDevice) bool { return d != nil }) {
			a.cancel()
		}
	}
}

func (h *wsHub) closeApp(a *appInstance, now time.Time) {
	for _, d := range a.devices {
		if d != nil {
			h.clearView(d)
			h.render(d, now, false)
		}
	}
}

func (h *wsHub) tickApps(now time.Time) {
	seen := map[*appInstance]bool{}
	for _, d := range h.devices {
		a := d.app
		if a == nil || seen[a] || a.remaining == 0 || a.tickQueued || now.Before(a.nextTick) || !h.active(d) {
			continue
		}
		seen[a] = true
		if h.updateApp(a, appEvent{kind: "tick", i: -1, now: now}) {
			a.remaining--
			a.nextTick = now.Add(time.Duration(a.spec.TickMS) * time.Millisecond)
		}
	}
}

func (h *wsHub) updateApp(a *appInstance, ev appEvent) bool {
	if ev.kind == "tap" && ev.id == exitNode {
		h.closeApp(a, ev.now)
		return true
	}
	if (ev.kind != "patch" && a.hooks[ev.kind] == nil) || len(a.events) >= 32 {
		return false
	}
	a.events = append(a.events, ev)
	if ev.kind == "tick" {
		a.tickQueued = true
	}
	if !a.running {
		a.running = true
		go h.runAppEvents(a)
	}
	return true
}

func (h *wsHub) runAppEvents(a *appInstance) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(a.events) != 0 && !h.closed && a.ctx.Err() == nil {
		ev := a.events[0]
		a.events = a.events[1:]
		if ev.i >= 0 && (a.devices[ev.i] == nil || !h.active(a.devices[ev.i])) {
			continue
		}
		var connected [8]bool
		for i, d := range a.devices {
			connected[i] = d != nil && h.active(d)
		}
		h.mu.Unlock()
		if ev.kind == "patch" {
			views, snapshot, err := a.applyStatePatch(ev.patch, connected[:len(a.devices)])
			h.mu.Lock()
			if err != nil {
				ev.done <- appPatchResult{err: problem(400, err.Error())}
			} else if !h.closed && a.ctx.Err() == nil {
				a.snapshot = snapshot
				h.publishAppViews(a, views)
				active := h.activeAppState(a)
				ev.done <- appPatchResult{out: object{"ok": true, "devices": active["devices"], "title": a.spec.Title, "state": snapshot, "hit_ids": appHitIDs(views...)}}
			} else {
				ev.done <- appPatchResult{err: problem(409, "App closed before applying state_patch")}
			}
			continue
		}
		views, err := a.update(ev, connected[:len(a.devices)])
		var snapshot json.RawMessage
		if err == nil && views != nil {
			snapshot, err = a.stateJSON()
		}
		if err == nil {
			err = h.storeAppEffects(a)
		}
		h.mu.Lock()
		if ev.kind == "tick" {
			a.tickQueued = false
		}
		if !h.closed && a.ctx.Err() == nil {
			if snapshot != nil {
				a.snapshot = snapshot
			}
			h.commitAppEvent(a, ev, views, err)
		}
	}
	a.running, a.events = false, nil
}

func (h *wsHub) storeAppEffects(a *appInstance) error {
	effects := a.effects
	if i := slices.IndexFunc(effects, func(e appEffect) bool { return e.kind == "exit" }); i >= 0 {
		effects = effects[:i]
	}
	if a.ctx.Err() != nil || !slices.ContainsFunc(effects, func(e appEffect) bool { return e.kind == "store" }) {
		return nil
	}
	if h.b.db == nil {
		return errors.New("App storage unavailable")
	}
	ctx, cancel := context.WithTimeout(a.ctx, 200*time.Millisecond)
	defer cancel()
	tx, err := h.b.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("EchoEar could not open storage for app %q: %v", a.spec.Name, err)
		return errors.New("Could not save app storage")
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, e := range effects {
		if e.kind != "store" {
			continue
		}
		edit := memoryEdit{memoryWrite: memoryWrite{a.scope, e.text, string(e.value)}}
		if _, err := editMemory(ctx, tx, edit, now); err != nil {
			log.Printf("EchoEar could not persist app %q key %q: %v", a.spec.Name, e.text, err)
			return fmt.Errorf("Could not save app key %q", e.text)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("EchoEar could not commit storage for app %q: %v", a.spec.Name, err)
		return errors.New("Could not save app storage")
	}
	for _, e := range effects {
		if e.kind == "store" {
			a.memory[e.text] = e.value
		}
	}
	return nil
}

func (h *wsHub) commitAppEvent(a *appInstance, ev appEvent, views []svgScreen, err error) {
	now := ev.now
	if err != nil {
		for _, d := range a.devices {
			if d != nil && h.active(d) {
				h.push(d, object{"op": "error", "status": 400, "error": err.Error()}, true)
			}
		}
		h.closeApp(a, now)
		return
	}
	effects := a.effects
	a.effects = nil
	for _, e := range effects {
		if e.kind == "exit" {
			h.closeApp(a, now)
			return
		}
		if e.kind == "store" {
			continue
		}
		if e.kind == "say" {
			if now.Sub(a.timeOfSpeech) < 3*time.Second {
				continue
			}
			a.timeOfSpeech = now
		}
		for j, d := range a.devices {
			if d == nil || !h.active(d) || ev.i >= 0 && j != ev.i && !e.all {
				continue
			}
			switch e.kind {
			case "chime":
				h.chime(d, e.text)
			case "pcm":
				h.playPCM(d, e.pcm)
			case "say":
				h.speakLocal(d, e.text)
			}
		}
	}
	h.publishAppViews(a, views)
}

func (h *wsHub) publishAppViews(a *appInstance, views []svgScreen) {
	for j, screen := range views {
		if d := a.devices[j]; d != nil && h.active(d) && screen.source != "" && d.appView.raw != screen.raw {
			d.appView = screen
			h.render(d, time.Now(), false)
		}
	}
}

func withDevice(args object, id string) object {
	out := maps.Clone(args)
	out["device_id"] = id
	return out
}
