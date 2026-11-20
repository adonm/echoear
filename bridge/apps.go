package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

func (b *bridge) listApps(ctx context.Context) ([]object, error) {
	out := []object{}
	if b.db == nil {
		return out, nil
	}
	rows, err := b.db.QueryContext(ctx, `SELECT name,title,kind,author,updated_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, title, kind, author string
		var updated int64
		if err := rows.Scan(&name, &title, &kind, &author, &updated); err != nil {
			return nil, err
		}
		out = append(out, object{"name": name, "title": title, "kind": kind, "author": author, "updated_at": updated})
	}
	return out, rows.Err()
}

func (b *bridge) appAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/apps" && r.Method == http.MethodGet:
		list, err := b.listApps(ctx)
		return object{"apps": list}, err
	case path == "/api/apps/run" && r.Method == http.MethodPost:
		data, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		return b.runApp(ctx, object{"devices": data["device_id"], "load": data["name"]})
	case r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/apps/")
		if !slugRE.MatchString(name) {
			return nil, problem(400, "Invalid app name")
		}
		if b.db == nil {
			return nil, problem(503, "Apps unavailable")
		}
		_, err := deleteRow(ctx, b.db, "DELETE FROM apps WHERE name=?", name)
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
	spec                                           savedApp
	players                                        []appPlayer
	devices                                        []*wsDevice
	vm                                             *goja.Runtime
	self, state                                    goja.Value
	render, tap, tick, parse, stringify, errorText goja.Callable
	scope                                          string
	memory                                         map[string]json.RawMessage
	effects                                        []appEffect
	allowEffects                                   bool
	geometry                                       []screenGeometry
	lastScreens                                    []svgScreen
	ctx                                            context.Context
	cancel                                         context.CancelFunc
	events                                         []appEvent
	running, tickQueued                            bool
	nextTick, timeOfSpeech                         time.Time
	remaining                                      int
}

type appEvent struct {
	i, x, y int
	id      string
	now     time.Time
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

func initApp(spec savedApp, players []appPlayer, memory map[string]json.RawMessage, devices []*wsDevice) (*appInstance, []svgScreen, error) {
	a := &appInstance{spec: spec, players: players, memory: memory, devices: devices, scope: "app:" + spec.Name,
		geometry: make([]screenGeometry, len(players)), lastScreens: make([]svgScreen, len(players))}
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
		if err := a.initVM(); err != nil {
			return nil, nil, err
		}
	}
	screens, err := a.screens(nil)
	return a, screens, err
}

func (a *appInstance) initVM() error {
	vm := goja.New()
	a.vm = vm
	vm.SetMaxCallStackSize(512)
	a.errorText, _ = goja.AssertFunction(vm.Get("String"))
	hostJSON := vm.Get("JSON").ToObject(vm)
	a.parse, _ = goja.AssertFunction(hostJSON.Get("parse"))
	a.stringify, _ = goja.AssertFunction(hostJSON.Get("stringify"))
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
				panic(vm.NewTypeError("%s is only available in onTap/onTick", name))
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
	run("storeGet", func(c goja.FunctionCall) goja.Value {
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
			if (app && typeof app === "object") return [app, app.init, app.render, app.onTap, app.onTick];
			return [void 0, typeof init === "function" ? init : void 0, typeof render === "function" ? render : void 0,
				typeof onTap === "function" ? onTap : void 0, typeof onTick === "function" ? onTick : void 0];
		})()`)
	})
	if err != nil {
		return err
	}
	obj := methods.(*goja.Object)
	a.self = obj.Get("0")
	init, _ := goja.AssertFunction(obj.Get("1"))
	a.render, _ = goja.AssertFunction(obj.Get("2"))
	a.tap, _ = goja.AssertFunction(obj.Get("3"))
	a.tick, _ = goja.AssertFunction(obj.Get("4"))
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
		screen, err := buildScreen(raw, true, a.geometry[i])
		if err != nil {
			return nil, fmt.Errorf("render for player %d: %w", i, err)
		}
		a.lastScreens[i], out[i] = screen, screen
	}
	return out, nil
}

func (a *appInstance) update(i int, id string, x, y int, connected []bool) ([]svgScreen, error) {
	fn := a.tick
	if i >= 0 {
		fn = a.tap
	}
	if fn == nil {
		return nil, nil
	}
	args := []goja.Value{a.state}
	if i >= 0 {
		args = append(args, a.player(i), a.vm.ToValue(id), a.vm.ToValue(x), a.vm.ToValue(y))
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

func (b *bridge) runApp(ctx context.Context, args object) (object, error) {
	ids, err := appDevices(args["devices"])
	if err != nil {
		return nil, err
	}
	h := b.ws
	if h == nil {
		return nil, problem(503, "Device hub unavailable")
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
			if _, err := buildScreen(a.SVG, false, geometry); err != nil {
				return nil, problem(400, err.Error())
			}
		}
	}
	memory := map[string]json.RawMessage{}
	if b.db != nil && a.Code != "" {
		values, err := b.memory(ctx, object{"op": "list", "scope": "app:" + a.Name}, true)
		if err != nil {
			return nil, err
		}
		for _, value := range values["entries"].([]object) {
			memory[str(value["key"])] = value["value"].(json.RawMessage)
		}
	}
	inst, screens, err := initApp(a, players, memory, devices)
	if err != nil {
		return nil, problem(400, err.Error())
	}
	if save != "" {
		if err := b.saveApp(ctx, a); err != nil {
			return nil, err
		}
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
	inst.ctx, inst.cancel = context.WithCancel(b.root)
	now := time.Now()
	if inst.tick != nil && a.TickMS != 0 {
		inst.remaining = a.MaxTicks
		inst.nextTick = now.Add(time.Duration(a.TickMS) * time.Millisecond)
	}
	seen := map[string]bool{}
	hits := []string{}
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
		for _, hit := range screens[i].hits {
			if !seen[hit.id] {
				seen[hit.id] = true
				hits = append(hits, hit.id)
			}
		}
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
	out := object{"ok": true, "devices": ids, "title": a.Title, "hit_ids": hits}
	if save != "" {
		out["saved_as"] = save
	}
	return out, nil
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
		if h.updateApp(a, -1, "", 0, 0, now) {
			a.remaining--
			a.nextTick = now.Add(time.Duration(a.spec.TickMS) * time.Millisecond)
		}
	}
}

func (h *wsHub) updateApp(a *appInstance, i int, id string, x, y int, now time.Time) bool {
	if id == exitNode {
		h.closeApp(a, now)
		return true
	}
	if i >= 0 && a.tap == nil || i < 0 && a.tick == nil || len(a.events) >= 32 {
		return false
	}
	a.events = append(a.events, appEvent{i, x, y, id, now})
	if i < 0 {
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
		views, err := a.update(ev.i, ev.id, ev.x, ev.y, connected[:len(a.devices)])
		if err == nil {
			h.storeAppEffects(a)
		}
		h.mu.Lock()
		if ev.i < 0 {
			a.tickQueued = false
		}
		if !h.closed && a.ctx.Err() == nil {
			h.commitAppEvent(a, ev, views, err)
		}
	}
	a.running, a.events = false, nil
}

func (h *wsHub) storeAppEffects(a *appInstance) {
	if h.b.db == nil {
		return
	}
	for _, e := range a.effects {
		if e.kind == "exit" || a.ctx.Err() != nil {
			return
		}
		if e.kind == "store" {
			ctx, cancel := context.WithTimeout(a.ctx, 200*time.Millisecond)
			_, err := h.b.db.setMemory(ctx, memoryWrite{a.scope, e.text, string(e.value)}, nil)
			cancel()
			if err == nil {
				a.memory[e.text] = e.value
			}
		}
	}
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
