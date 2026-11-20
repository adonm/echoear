package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	maxSceneStates = 8
	maxSceneTimers = 64
	maxPatchDepth  = 3
	maxDelayDepth  = 2
)

type tapReaction struct {
	path                     string
	chime, speak, state, ask string
	notes                    []string
	pcm                      []byte
	exit                     bool
	broadcast                bool
	store                    *memoryWrite
	patch                    *scenePatch
	move                     *nodeMove
	delay, revert            time.Duration
	then                     *tapReaction
}

type reactionCycle []tapReaction

type nodeMove struct {
	target string
	dx, dy int
}

type nodeEdit struct {
	visual, controls object
	group            *string
}

type scenePatch struct {
	path    string
	bg      string
	set     map[string]nodeEdit
	add     []nodeEdit
	del     map[string]bool
	delPath map[string]string
}

func validSceneID(id string) bool { return len(id) <= 24 && deviceIDRE.MatchString(id) }

func fieldError(path, format string, args ...any) error {
	return problem(400, path+": "+fmt.Sprintf(format, args...))
}

func fieldPath(base, key string) string {
	if base == "" {
		return key
	}
	return base + "." + key
}

func parseReaction(raw object, states map[string]sceneView, patchDepth, delayDepth int, path string) (tapReaction, error) {
	r := tapReaction{path: path}
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		value, field := raw[key], path+"."+key
		switch key {
		case "set", "add", "del":
			return r, fieldError(field, `wrap inside tap.patch (example: tap: {patch: {set: {"id": {"c": "#34D399"}}}})`)
		case "c", "x", "y", "s", "vis":
			return r, fieldError(field, `visual updates go inside tap.patch.set["<node_id>"], not directly on tap`)
		case "chime", "speak", "state", "ask":
			if _, ok := value.(string); !ok {
				return r, fieldError(field, "expected a string")
			}
		case "exit", "broadcast":
			flag, ok := value.(bool)
			if !ok {
				return r, fieldError(field, "expected a boolean")
			}
			if key == "exit" {
				r.exit = flag
			} else {
				r.broadcast = flag
			}
		case "store":
			fields, ok := value.(object)
			if !ok || len(fields) != 3 {
				return r, fieldError(field, "expected scope, key and value")
			}
			write, err := parseMemoryWrite(fields)
			if err != nil {
				return r, fieldError(field, "%s", err)
			}
			r.store = &write
		case "patch":
			p, ok := value.(object)
			if !ok || patchDepth >= maxPatchDepth {
				return r, fieldError(field, "expected a patch object with at most %d nested patches", maxPatchDepth)
			}
			for field := range p {
				switch field {
				case "bg", "set", "add", "del":
				default:
					return r, fieldError(path+".patch."+field, "use bg, set, add or del")
				}
			}
			var err error
			r.patch, err = parseScenePatchAt(p, states, patchDepth+1, field, field+".add")
			if err != nil {
				return r, err
			}
		case "move":
			move, ok := value.(object)
			if !ok || len(move) != 3 {
				return r, fieldError(field, "expected target, dx and dy")
			}
			target := str(move["target"])
			dx, xOK := moveDelta(move["dx"])
			dy, yOK := moveDelta(move["dy"])
			if strings.TrimSpace(target) == "" {
				return r, fieldError(field+".target", "use a node ID or group name")
			}
			if !xOK {
				return r, fieldError(field+".dx", "expected an integer")
			}
			if !yOK {
				return r, fieldError(field+".dy", "expected an integer")
			}
			r.move = &nodeMove{target, dx, dy}
		case "notes":
			var err error
			r.notes, r.pcm, err = parseMelody(value, field)
			if err != nil {
				return r, err
			}
		case "after_ms", "then", "revert_ms":
		default:
			return r, fieldError(field, "unknown reaction field")
		}
	}
	r.speak, r.ask = shortText(raw["speak"], 240, true), shortText(raw["ask"], 240, true)
	if _, ok := raw["chime"]; ok {
		r.chime = str(raw["chime"])
		if !validChime(r.chime) {
			return r, fieldError(path+".chime", "use tap, think, intercom or celebrate")
		}
	}
	if _, ok := raw["state"]; ok {
		r.state = str(raw["state"])
		if _, found := states[r.state]; !found {
			return r, fieldError(path+".state", "unknown scene state %q", r.state)
		}
	}
	revert, err := mcpUint(raw, "revert_ms")
	if err != nil || revert != nil && (*revert < 50 || *revert > 10000) {
		return r, fieldError(path+".revert_ms", "expected 50–10000 ms")
	}
	if revert != nil {
		r.revert = time.Duration(*revert) * time.Millisecond
	}
	ms, err := mcpUint(raw, "after_ms")
	then, delayed := raw["then"]
	if err != nil || (ms != nil) != delayed {
		return r, fieldError(path+".after_ms", "use an integer and supply then together")
	}
	if ms != nil {
		next, ok := then.(object)
		if !ok || *ms < 50 || *ms > 10000 || delayDepth >= maxDelayDepth {
			return r, fieldError(path+".then", "use a reaction object with after_ms 50–10000; at most two nested delays")
		}
		reaction, err := parseReaction(next, states, patchDepth, delayDepth+1, path+".then")
		if err != nil {
			return r, err
		}
		r.delay, r.then = time.Duration(*ms)*time.Millisecond, &reaction
	}
	return r, nil
}

func moveDelta(value any) (int, bool) {
	switch n := value.(type) {
	case json.Number:
		delta, err := n.Int64()
		return int(min(359, max(-359, delta))), err == nil
	case int:
		return min(359, max(-359, n)), true
	}
	return 0, false
}

func parseNodeEdit(raw object, states map[string]sceneView, partial bool, depth int, path string) (nodeEdit, error) {
	visual, err := visualNodeAt(raw, partial, path)
	edit := nodeEdit{visual: visual}
	if err != nil {
		return edit, err
	}
	if value, found := raw["group"]; found {
		group, ok := value.(string)
		if !ok && value != nil {
			return edit, fieldError(path+".group", "expected a group name or null")
		}
		edit.group = &group
	}
	for _, key := range []string{"on", "action", "ask", "tap", "cycle"} {
		value, found := raw[key]
		if !found {
			continue
		}
		if edit.controls == nil {
			edit.controls = object{}
		}
		switch key {
		case "on", "action", "ask":
			s, ok := value.(string)
			if !ok || key == "on" && s != "" && s != "tap" || key == "action" && s != "" && s != "exit" {
				return edit, fieldError(path+"."+key, "invalid node control")
			}
			value = shortText(s, 120, true)
		case "tap":
			switch v := value.(type) {
			case object:
				r, err := parseReaction(v, states, depth, 0, path+".tap")
				if err != nil {
					return edit, err
				}
				value = &r
			case string:
				value = shortText(v, 120, true) // Legacy spoken feedback.
			case nil: // Clear the declaration on a keyed patch.
			default:
				return edit, fieldError(path+".tap", "expected a reaction object")
			}
		case "cycle":
			if value == nil && partial {
				break
			}
			items, ok := value.([]any)
			if !ok || len(items) < 2 || len(items) > 8 {
				return edit, fieldError(path+".cycle", "expected an array of 2-8 reaction objects")
			}
			cycle := make(reactionCycle, 0, len(items))
			for i, item := range items {
				raw, ok := item.(object)
				if !ok {
					return edit, fieldError(path+".cycle", "expected an array of 2-8 reaction objects")
				}
				reaction, err := parseReaction(raw, states, depth, 0, fmt.Sprintf("%s.cycle[%d]", path, i))
				if err != nil {
					return edit, err
				}
				cycle = append(cycle, reaction)
			}
			value = &cycle
		}
		edit.controls[key] = value
	}
	return edit, nil
}

func parseScenePatch(raw object, states map[string]sceneView, depth int) (*scenePatch, error) {
	return parseScenePatchAt(raw, states, depth, "", "add")
}

func parseScenePatchAt(raw object, states map[string]sceneView, depth int, path, addPath string) (*scenePatch, error) {
	p := &scenePatch{path: path}
	if c, ok := raw["bg"]; ok {
		p.bg = color(c)
		if p.bg == "" {
			return nil, fieldError(fieldPath(path, "bg"), "expected #RRGGBB")
		}
	}
	if raw["set"] != nil {
		set, ok := raw["set"].(object)
		if !ok {
			return nil, fieldError(fieldPath(path, "set"), "expected an object keyed by node ID")
		}
		p.set = make(map[string]nodeEdit, len(set))
		for id, raw := range set {
			field := fieldPath(path, "set."+id)
			fields, ok := raw.(object)
			if !validSceneID(id) || !ok {
				return nil, fieldError(field, "use a valid node ID and an object of changes")
			}
			if _, hasID := fields["id"]; hasID {
				return nil, fieldError(field+".id", "do not change the node ID in set")
			}
			edit, err := parseNodeEdit(fields, states, true, depth, field)
			if err != nil {
				return nil, err
			}
			p.set[id] = edit
		}
	}
	add, err := listNodesAt(raw["add"], addPath)
	if err != nil {
		return nil, err
	}
	for i, node := range add {
		edit, err := parseNodeEdit(node, states, false, depth, fmt.Sprintf("%s[%d]", addPath, i))
		if err != nil {
			return nil, err
		}
		p.add = append(p.add, edit)
	}
	if raw["del"] != nil {
		del, ok := raw["del"].([]any)
		if !ok {
			return nil, fieldError(fieldPath(path, "del"), "expected an array of node IDs")
		}
		p.del = make(map[string]bool, len(del))
		p.delPath = make(map[string]string, len(del))
		for i, v := range del {
			id := str(v)
			field := fmt.Sprintf("%s[%d]", fieldPath(path, "del"), i)
			if !validSceneID(id) {
				return nil, fieldError(field, "expected a valid node ID")
			}
			p.del[id] = true
			p.delPath[id] = field
		}
	}
	return p, nil
}

func parseScene(raw object) (sceneView, map[string]sceneView, error) {
	screens, ok := raw["states"].(object)
	if raw["states"] != nil && (!ok || len(screens) > maxSceneStates) {
		return sceneView{}, nil, problem(400, "Expected up to 8 named scene states")
	}
	states := make(map[string]sceneView, len(screens))
	for name := range screens {
		if !validSceneID(name) {
			return sceneView{}, nil, problem(400, "Invalid scene state name")
		}
		states[name] = sceneView{} // Forward references are allowed.
	}
	for name, raw := range screens {
		screen, ok := raw.(object)
		if !ok || screen["nodes"] == nil {
			return sceneView{}, nil, problem(400, "Expected a state with nodes")
		}
		view, err := parseScreen(screen, states, "states."+name)
		if err != nil {
			return sceneView{}, nil, err
		}
		states[name] = view
	}
	view, err := parseScreen(raw, states, "")
	if err != nil {
		return view, nil, err
	}
	targets := newSceneTargets(view, states)
	if err = targets.checkView(view); err != nil {
		return view, nil, err
	}
	for _, name := range slices.Sorted(maps.Keys(states)) {
		if err = targets.checkView(states[name]); err != nil {
			return view, nil, err
		}
	}
	return view, states, nil
}

func parseScreen(raw object, states map[string]sceneView, path string) (sceneView, error) {
	view := sceneView{bg: "#0B1020"}
	if title := shortText(raw["title"], 48, false); title != "" {
		view.title = title
		view.nodes = append(view.nodes, label("echoear-title", 180, 38, 2, title, "#FFF1C8"))
	}
	args := object{"add": raw["nodes"]}
	if bg, ok := raw["bg"]; ok {
		args["bg"] = bg
	}
	patch, err := parseScenePatchAt(args, states, 0, path, fieldPath(path, "nodes"))
	if err != nil {
		return sceneView{}, err
	}
	return patch.apply(view)
}

type sceneTargets struct {
	ids, groups map[string]bool
}

func newSceneTargets(view sceneView, states map[string]sceneView) sceneTargets {
	t := sceneTargets{map[string]bool{}, map[string]bool{}}
	t.addView(view)
	for _, state := range states {
		t.addView(state)
	}
	return t
}

func (t sceneTargets) addView(view sceneView) {
	for _, n := range view.nodes {
		t.ids[str(n["id"])] = true
	}
	for _, g := range view.groups {
		t.groups[g] = true
	}
	for _, c := range view.controls {
		t.collectControls(c)
	}
}

func (t sceneTargets) collectControls(c object) {
	if r, ok := c["tap"].(*tapReaction); ok {
		t.collectReaction(r)
	}
	if cycle, ok := c["cycle"].(*reactionCycle); ok {
		for i := range *cycle {
			t.collectReaction(&(*cycle)[i])
		}
	}
}

func (t sceneTargets) collectReaction(r *tapReaction) {
	for ; r != nil; r = r.then {
		if r.patch != nil {
			t.collectPatch(r.patch)
		}
	}
}

func (t sceneTargets) collectPatch(p *scenePatch) {
	for _, e := range p.add {
		t.ids[str(e.visual["id"])] = true
		if e.group != nil && *e.group != "" {
			t.groups[*e.group] = true
		}
		t.collectControls(e.controls)
	}
	for _, e := range p.set {
		if e.group != nil && *e.group != "" {
			t.groups[*e.group] = true
		}
		t.collectControls(e.controls)
	}
}

func (t sceneTargets) checkView(view sceneView) error {
	for _, n := range view.nodes {
		if err := t.checkControls(view.controls[str(n["id"])]); err != nil {
			return err
		}
	}
	return nil
}

func (t sceneTargets) checkControls(c object) error {
	if r, ok := c["tap"].(*tapReaction); ok {
		if err := t.checkReaction(r); err != nil {
			return err
		}
	}
	if cycle, ok := c["cycle"].(*reactionCycle); ok {
		for i := range *cycle {
			if err := t.checkReaction(&(*cycle)[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t sceneTargets) checkReaction(r *tapReaction) error {
	for ; r != nil; r = r.then {
		if r.move != nil && !t.ids[r.move.target] && !t.groups[r.move.target] {
			return fieldError(r.path+".move.target", "node or group %q not found in scene", r.move.target)
		}
		if r.patch != nil {
			if err := t.checkPatch(r.patch); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t sceneTargets) checkPatch(p *scenePatch) error {
	for _, id := range slices.Sorted(maps.Keys(p.set)) {
		if !t.ids[id] {
			return fieldError(fieldPath(p.path, "set."+id), "node %q not found in scene", id)
		}
		if err := t.checkControls(p.set[id].controls); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(p.del)) {
		if !t.ids[id] {
			return fieldError(p.delPath[id], "node %q not found in scene", id)
		}
	}
	for _, e := range p.add {
		if err := t.checkControls(e.controls); err != nil {
			return err
		}
	}
	return nil
}

func (p *scenePatch) apply(old sceneView) (sceneView, error) {
	next := sceneView{bg: old.bg, title: old.title, nodes: make([]object, 0, min(maxNodes, len(old.nodes)+len(p.add)+1)),
		actions: make(map[string]sceneAction), controls: make(map[string]object), feedback: old.feedback}
	if p.bg != "" {
		next.bg = p.bg
	}
	seen := map[string]bool{}
	appendNode := func(n object, a sceneAction, controls object, group string) error {
		id := str(n["id"])
		if seen[id] {
			return problem(400, "Duplicate node ID "+id)
		}
		seen[id] = true
		if len(next.nodes) < maxNodes {
			next.nodes = append(next.nodes, n)
			if a.kind != "" {
				next.actions[id] = a
			}
			if controls != nil {
				next.controls[id] = controls
			}
			if group != "" {
				if next.groups == nil {
					next.groups = map[string]string{}
				}
				next.groups[id] = group
			}
		}
		return nil
	}
	for _, n := range old.nodes {
		id := str(n["id"])
		if id == exitNode || p.del[id] {
			continue
		}
		a, controls, group := old.actions[id], old.controls[id], old.groups[id]
		if edit, ok := p.set[id]; ok {
			n = maps.Clone(n)
			maps.Copy(n, edit.visual)
			if edit.visual["t"] == "tri" && n["fill"] == nil {
				n["fill"] = true
			}
			if edit.group != nil {
				group = *edit.group
			}
			if edit.controls != nil {
				updated := object{}
				maps.Copy(updated, controls)
				for k, v := range edit.controls {
					if v == nil {
						delete(updated, k)
					} else {
						updated[k] = v
					}
				}
				controls = updated
				a = nodeTapAction(n, controls)
			}
		}
		if err := appendNode(n, a, controls, group); err != nil {
			return sceneView{}, err
		}
	}
	for id := range p.set {
		if !seen[id] {
			return sceneView{}, fieldError(fieldPath(p.path, "set."+id), "node %q not found in scene", id)
		}
	}
	for _, edit := range p.add {
		n := maps.Clone(edit.visual)
		group := ""
		if edit.group != nil {
			group = *edit.group
		}
		if err := appendNode(n, nodeTapAction(n, edit.controls), edit.controls, group); err != nil {
			return sceneView{}, err
		}
	}
	for _, n := range next.nodes {
		id := str(n["id"])
		r, _ := next.controls[id]["tap"].(*tapReaction)
		if n["vis"] != false && (next.actions[id].kind == "cat" || next.controls[id]["action"] == "exit" || r != nil && r.exit) {
			return next, nil
		}
	}
	if len(next.nodes) == maxNodes {
		id := str(next.nodes[maxNodes-1]["id"])
		delete(next.actions, id)
		delete(next.controls, id)
		delete(next.groups, id)
		next.nodes = next.nodes[:maxNodes-1]
	}
	next.nodes = append(next.nodes, comicButton(exitNode, 139, 300, 82, 32, "Cat", "#FFFAEE"))
	next.actions[exitNode] = sceneAction{"cat", ""}
	return next, nil
}

func (m nodeMove) apply(view *sceneView) bool {
	moved := false
	for i, n := range view.nodes {
		id := str(n["id"])
		if id != m.target && view.groups[id] != m.target {
			continue
		}
		keys := []string{"x", "y"}
		switch n["t"] {
		case "line":
			keys = []string{"x1", "y1", "x2", "y2"}
		case "tri":
			keys = []string{"x1", "y1", "x2", "y2", "x3", "y3"}
		}
		var updated object
		for j, key := range keys {
			delta := [2]int{m.dx, m.dy}[j%2]
			old := nodeInt(n, key)
			if v := min(359, max(0, old+delta)); v != old {
				if updated == nil {
					updated = maps.Clone(n)
				}
				updated[key] = v
			}
		}
		if updated != nil {
			if !moved {
				view.nodes = slices.Clone(view.nodes)
				moved = true
			}
			view.nodes[i] = updated
		}
	}
	return moved
}

type sceneRevert struct {
	bg  string
	set map[string]object
}

func captureSceneRevert(before, after sceneView) sceneRevert {
	var undo sceneRevert
	if before.bg != after.bg {
		undo.bg = before.bg
	}
	nodes := make(map[string]object, len(before.nodes))
	for _, n := range before.nodes {
		nodes[str(n["id"])] = n
	}
	for _, n := range after.nodes {
		id := str(n["id"])
		if old := nodes[id]; old != nil {
			for key, value := range n {
				if key == "on" || value == old[key] {
					continue
				}
				if undo.set == nil {
					undo.set = map[string]object{}
				}
				if undo.set[id] == nil {
					undo.set[id] = object{}
				}
				undo.set[id][key] = old[key]
			}
		}
	}
	return undo
}

func (undo sceneRevert) apply(view sceneView) sceneView {
	if undo.bg != "" {
		view.bg = undo.bg
	}
	if len(undo.set) > 0 {
		view.nodes = slices.Clone(view.nodes)
		for i, n := range view.nodes {
			if fields := undo.set[str(n["id"])]; fields != nil {
				n = maps.Clone(n)
				for key, v := range fields {
					if v == nil {
						delete(n, key)
					} else {
						n[key] = v
					}
				}
				view.nodes[i] = n
			}
		}
	}
	return view
}

func nodeTapAction(n, controls object) sceneAction {
	delete(n, "on")
	a := sceneAction{}
	r, _ := controls["tap"].(*tapReaction)
	switch {
	case r != nil || controls["cycle"] != nil:
		a.kind = "local"
	case controls["action"] == "exit":
		a.kind = "cat"
	case str(controls["ask"]) != "":
		a = sceneAction{"ask", str(controls["ask"])}
	case str(controls["tap"]) != "":
		a = sceneAction{"tap", str(controls["tap"])}
	case controls["on"] == "tap" || n["t"] == "btn" && controls["on"] == nil:
		a.kind = "event"
	}
	if a.kind != "" {
		n["on"] = "tap"
	} else if controls["on"] == "" {
		n["on"] = ""
	}
	return a
}

type cyclePosition struct {
	cycle *reactionCycle
	index int
}

func (h *wsHub) stopSceneTimers(d *wsDevice) {
	for timer := range d.sceneTimers {
		timer.Stop()
	}
	clear(d.sceneTimers)
}

func (h *wsHub) clearLocalScene(d *wsDevice) {
	h.stopSceneTimers(d)
	d.states, d.cycles = nil, nil
	d.room = ""
}

func (h *wsHub) scheduleSceneTimer(d *wsDevice, delay time.Duration, run func(time.Time)) {
	if !d.customScene || len(d.sceneTimers) >= maxSceneTimers {
		return
	}
	if d.sceneTimers == nil {
		d.sceneTimers = make(map[*time.Timer]struct{})
	}
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		// Membership identifies the scene generation even if Stop lost
		// the race with a callback waiting to acquire the hub lock.
		if _, ok := d.sceneTimers[timer]; ok {
			delete(d.sceneTimers, timer)
			if h.active(d) && d.customScene {
				run(time.Now())
			}
		}
	})
	d.sceneTimers[timer] = struct{}{}
}

func (h *wsHub) showCustomScene(d *wsDevice, view sceneView, now time.Time, full bool) {
	d.pending, d.card, d.drawer, d.agentSetScene = nil, nil, "", d.busy
	h.wake(d, now)
	h.updateCustomScene(d, view, now, full)
}

func (h *wsHub) updateCustomScene(d *wsDevice, view sceneView, now time.Time, full bool) {
	d.view, d.customScene = &view, true
	before := d.rev
	h.render(d, now, full)
	if d.rev == before {
		d.rev++
		h.push(d, object{"op": "patch", "rev": d.rev, "set": object{}, "add": []object{}, "del": []string{}}, true)
	}
	d.viewRev = d.rev
}

func (h *wsHub) patchScene(d *wsDevice, patch *scenePatch, move *nodeMove, now time.Time) error {
	view, _ := d.sceneState()
	next := view
	if patch != nil {
		var err error
		next, err = patch.apply(view)
		if err != nil {
			return err
		}
	}
	if move != nil && !move.apply(&next) && patch == nil {
		return nil
	}
	for id, pos := range d.cycles {
		if pos.cycle != next.controls[id]["cycle"] {
			delete(d.cycles, id)
		}
	}
	if d.customScene {
		h.updateCustomScene(d, next, now, false)
	} else {
		h.showCustomScene(d, next, now, false)
	}
	return nil
}

func (h *wsHub) localTap(d *wsDevice, id string, event object, waiting bool, now time.Time) {
	controls := d.scene.controls[id]
	r, _ := controls["tap"].(*tapReaction)
	cycle, _ := controls["cycle"].(*reactionCycle)
	var step *tapReaction
	var pos cyclePosition
	if cycle != nil {
		pos = d.cycles[id]
		if pos.cycle != cycle {
			pos = cyclePosition{cycle: cycle}
		}
		step = &(*cycle)[pos.index]
		pos.index = (pos.index + 1) % len(*cycle)
		setCyclePosition(d, id, pos)
	}
	if d.room != "" && (r != nil && r.broadcast || step != nil && step.broadcast) {
		for _, other := range h.devices {
			if other == d || other.room != d.room || !h.active(other) {
				continue
			}
			h.emit(other.who.DeviceID, "tap", event)
			if step != nil && step.broadcast {
				if otherCycle, ok := other.view.controls[id]["cycle"].(*reactionCycle); ok && len(*otherCycle) == len(*cycle) {
					setCyclePosition(other, id, cyclePosition{otherCycle, pos.index})
				}
			}
		}
	}
	if r != nil && h.react(d, *r, waiting, now) || step != nil && h.react(d, *step, waiting, now) {
		return
	}
	if controls["action"] == "exit" {
		h.leaveScene(d, waiting, now)
	}
}

func setCyclePosition(d *wsDevice, id string, pos cyclePosition) {
	if d.cycles == nil {
		d.cycles = make(map[string]cyclePosition)
	}
	d.cycles[id] = pos
}

func (h *wsHub) react(d *wsDevice, r tapReaction, waiting bool, now time.Time) bool {
	if w := r.store; w != nil {
		if h.b.db == nil {
			h.push(d, object{"op": "error", "status": 503, "error": "Memory unavailable"}, true)
			return false
		}
		if _, err := h.b.db.setMemory(d.client.ctx, *w, nil); err != nil {
			h.push(d, object{"op": "error", "status": 502, "error": "Cannot store tap memory"}, true)
			return false
		}
	}
	if r.broadcast && d.room != "" {
		local := r
		local.ask, local.exit, local.then = "", false, nil
		for _, other := range h.devices {
			if other != d && other.room == d.room && h.active(other) {
				h.applyReaction(other, local, true, now)
			}
		}
	}
	return h.applyReaction(d, r, waiting, now)
}

func (h *wsHub) applyReaction(d *wsDevice, r tapReaction, waiting bool, now time.Time) bool {
	if r.chime != "" {
		h.chime(d, r.chime)
	}
	if len(r.pcm) != 0 {
		h.playPCM(d, r.pcm)
	}
	if r.state != "" {
		if view, ok := d.states[r.state]; ok {
			h.stopSceneTimers(d)
			d.cycles = nil
			h.showCustomScene(d, view, now, true)
		}
	}
	var undo sceneRevert
	if r.patch != nil || r.move != nil {
		before, _ := d.sceneState()
		if err := h.patchScene(d, r.patch, r.move, now); err != nil {
			h.push(d, object{"op": "error", "status": 400, "error": err.Error()}, true)
		} else if r.revert != 0 && !r.exit {
			after, _ := d.sceneState()
			undo = captureSceneRevert(before, after)
		}
	}
	if r.exit {
		h.leaveScene(d, waiting, now)
	}
	if r.ask != "" && !waiting {
		h.startTurn(d, now, true, func(ctx context.Context, id uint64) { h.runTurn(ctx, d, id, r.ask, "tap", nil) })
	}
	if r.speak != "" {
		h.speakLocal(d, r.speak)
	}
	if undo.bg != "" || len(undo.set) != 0 {
		h.scheduleSceneTimer(d, r.revert, func(now time.Time) {
			h.updateCustomScene(d, undo.apply(*d.view), now, false)
		})
	}
	if r.then != nil && !r.exit {
		h.scheduleSceneTimer(d, r.delay, func(now time.Time) {
			h.react(d, *r.then, h.hasWaiter(d), now)
		})
	}
	return r.exit
}

func (h *wsHub) speakLocal(d *wsDevice, text string) {
	h.speakLocalAfter(d, text, nil)
}

func (h *wsHub) speakLocalAfter(d *wsDevice, text string, prefix []byte) {
	worker := h.b.worker
	if worker == nil || !worker.ready.Load() {
		if len(prefix) != 0 {
			h.playPCM(d, prefix)
		}
		return
	}
	voice := h.b.voiceFor(d.who)
	stream := newAudioStream()
	if len(prefix) != 0 {
		stream.append(prefix)
	}
	playback := h.play(d, stream)
	go func() {
		ctx, cancel := context.WithTimeout(playback, ttsWait)
		defer cancel()
		pcm := h.b.clips.peek(voice, text)
		if pcm == nil {
			pcm, _ = worker.synthesize(ctx, voice, text)
		}
		if !stream.append(pcm) {
			stream.finish(errTTS)
		} else {
			stream.finish(nil)
		}
	}()
}
