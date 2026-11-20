package main

import (
	"context"
	"maps"
	"time"
)

const (
	maxSceneStates = 8
	maxSceneTimers = 64
	maxPatchDepth  = 3
	maxDelayDepth  = 2
)

type tapReaction struct {
	chime, speak, state, ask string
	exit                     bool
	patch                    *scenePatch
	delay                    time.Duration
	then                     *tapReaction
}

type reactionCycle []tapReaction

type nodeEdit struct{ visual, controls object }

type scenePatch struct {
	bg  string
	set map[string]nodeEdit
	add []nodeEdit
	del map[string]bool
}

func validSceneID(id string) bool { return len(id) <= 24 && deviceIDRE.MatchString(id) }

func parseReaction(raw object, states map[string]sceneView, patchDepth, delayDepth int) (tapReaction, error) {
	var r tapReaction
	for key, value := range raw {
		switch key {
		case "chime", "speak", "state", "ask":
			if _, ok := value.(string); !ok {
				return r, problem(400, "Expected a string for tap."+key)
			}
		case "exit":
			var ok bool
			if r.exit, ok = value.(bool); !ok {
				return r, problem(400, "Expected a boolean for tap.exit")
			}
		case "patch":
			p, ok := value.(object)
			if !ok || patchDepth >= maxPatchDepth {
				return r, problem(400, "Invalid or too deeply nested tap.patch")
			}
			for field := range p {
				switch field {
				case "bg", "set", "add", "del":
				default:
					return r, problem(400, "Unknown tap.patch field "+field)
				}
			}
			var err error
			r.patch, err = parseScenePatch(p, states, patchDepth+1)
			if err != nil {
				return r, err
			}
		case "after_ms", "then":
		default:
			return r, problem(400, "Unknown tap field "+key)
		}
	}
	r.speak, r.ask = shortText(raw["speak"], 240, true), shortText(raw["ask"], 240, true)
	if _, ok := raw["chime"]; ok {
		r.chime = str(raw["chime"])
		if !validChime(r.chime) {
			return r, problem(400, "Unknown tap chime kind")
		}
	}
	if _, ok := raw["state"]; ok {
		r.state = str(raw["state"])
		if _, found := states[r.state]; !found {
			return r, problem(400, "Unknown scene state "+r.state)
		}
	}
	ms, err := mcpUint(raw, "after_ms")
	then, delayed := raw["then"]
	if err != nil || (ms != nil) != delayed {
		return r, problem(400, "Use tap.after_ms and tap.then together")
	}
	if ms != nil {
		next, ok := then.(object)
		if !ok || *ms < 50 || *ms > 10000 || delayDepth >= maxDelayDepth {
			return r, problem(400, "Invalid or too deeply nested tap delay (50–10000 ms)")
		}
		reaction, err := parseReaction(next, states, patchDepth, delayDepth+1)
		if err != nil {
			return r, err
		}
		r.delay, r.then = time.Duration(*ms)*time.Millisecond, &reaction
	}
	return r, nil
}

func parseNodeEdit(raw object, states map[string]sceneView, partial bool, depth int) (nodeEdit, error) {
	visual, err := visualNode(raw, partial)
	edit := nodeEdit{visual: visual}
	if err != nil {
		return edit, err
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
				return edit, problem(400, "Invalid node "+key)
			}
			value = shortText(s, 120, true)
		case "tap":
			switch v := value.(type) {
			case object:
				r, err := parseReaction(v, states, depth, 0)
				if err != nil {
					return edit, err
				}
				value = &r
			case string:
				value = shortText(v, 120, true) // Legacy spoken feedback.
			case nil: // Clear the declaration on a keyed patch.
			default:
				return edit, problem(400, "Expected a tap reaction object")
			}
		case "cycle":
			if value == nil {
				break
			}
			items, ok := value.([]any)
			if !ok || len(items) < 2 || len(items) > 8 {
				return edit, problem(400, "Expected 2–8 reactions for cycle")
			}
			cycle := make(reactionCycle, 0, len(items))
			for _, item := range items {
				raw, ok := item.(object)
				if !ok {
					return edit, problem(400, "Expected a cycle reaction object")
				}
				reaction, err := parseReaction(raw, states, depth, 0)
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
	p := &scenePatch{}
	if c, ok := raw["bg"]; ok {
		p.bg = color(c)
		if p.bg == "" {
			return nil, problem(400, "Invalid scene background")
		}
	}
	if raw["set"] != nil {
		set, ok := raw["set"].(object)
		if !ok {
			return nil, problem(400, "Expected set keyed by node ID")
		}
		p.set = make(map[string]nodeEdit, len(set))
		for id, raw := range set {
			fields, ok := raw.(object)
			if !validSceneID(id) || !ok {
				return nil, problem(400, "Expected set keyed by valid node IDs")
			}
			if _, hasID := fields["id"]; hasID {
				return nil, problem(400, "Do not change node ID in set")
			}
			edit, err := parseNodeEdit(fields, states, true, depth)
			if err != nil {
				return nil, err
			}
			p.set[id] = edit
		}
	}
	add, err := listNodes(raw["add"])
	if err != nil {
		return nil, err
	}
	for _, node := range add {
		edit, err := parseNodeEdit(node, states, false, depth)
		if err != nil {
			return nil, err
		}
		p.add = append(p.add, edit)
	}
	if raw["del"] != nil {
		del, ok := raw["del"].([]any)
		if !ok {
			return nil, problem(400, "Expected del array of node IDs")
		}
		p.del = make(map[string]bool, len(del))
		for _, v := range del {
			id := str(v)
			if !validSceneID(id) {
				return nil, problem(400, "Invalid node ID in del")
			}
			p.del[id] = true
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
		view, err := parseScreen(screen, states)
		if err != nil {
			return sceneView{}, nil, err
		}
		states[name] = view
	}
	view, err := parseScreen(raw, states)
	return view, states, err
}

func parseScreen(raw object, states map[string]sceneView) (sceneView, error) {
	view := sceneView{bg: "#0B1020"}
	if title := shortText(raw["title"], 48, false); title != "" {
		view.nodes = append(view.nodes, label("echoear-title", 180, 38, 2, title, "#FFF1C8"))
	}
	args := object{"add": raw["nodes"]}
	if bg, ok := raw["bg"]; ok {
		args["bg"] = bg
	}
	patch, err := parseScenePatch(args, states, 0)
	if err != nil {
		return sceneView{}, err
	}
	return patch.apply(view)
}

func (p *scenePatch) apply(old sceneView) (sceneView, error) {
	next := sceneView{bg: old.bg, nodes: make([]object, 0, min(maxNodes, len(old.nodes)+len(p.add)+1)),
		actions: make(map[string]sceneAction), controls: make(map[string]object), feedback: old.feedback}
	if p.bg != "" {
		next.bg = p.bg
	}
	seen := map[string]bool{}
	appendNode := func(n object, a sceneAction, controls object) error {
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
		}
		return nil
	}
	for _, n := range old.nodes {
		id := str(n["id"])
		if id == exitNode || p.del[id] {
			continue
		}
		a, controls := old.actions[id], old.controls[id]
		if edit, ok := p.set[id]; ok {
			n = maps.Clone(n)
			maps.Copy(n, edit.visual)
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
		if err := appendNode(n, a, controls); err != nil {
			return sceneView{}, err
		}
	}
	for id := range p.set {
		if !seen[id] {
			return sceneView{}, problem(400, "Unknown node ID in set: "+id)
		}
	}
	for _, edit := range p.add {
		n := maps.Clone(edit.visual)
		if err := appendNode(n, nodeTapAction(n, edit.controls), edit.controls); err != nil {
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
		next.nodes = next.nodes[:maxNodes-1]
	}
	next.nodes = append(next.nodes, comicButton(exitNode, 139, 300, 82, 32, "Cat", "#FFFAEE"))
	next.actions[exitNode] = sceneAction{"cat", ""}
	return next, nil
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
}

func (h *wsHub) showCustomScene(d *wsDevice, view sceneView, now time.Time, full bool) {
	d.view, d.pending, d.drawer, d.customScene, d.agentSetScene = &view, nil, false, true, d.busy
	h.wake(d, now)
	before := d.rev
	h.render(d, now, full)
	if d.rev == before {
		d.rev++
		h.push(d, object{"op": "patch", "rev": d.rev, "set": object{}, "add": []object{}, "del": []string{}}, true)
	}
}

func (h *wsHub) patchScene(d *wsDevice, patch *scenePatch, now time.Time) error {
	view := d.scene
	if d.customScene && d.view != nil {
		view = *d.view
	}
	next, err := patch.apply(view)
	if err != nil {
		return err
	}
	for id, pos := range d.cycles {
		if pos.cycle != next.controls[id]["cycle"] {
			delete(d.cycles, id)
		}
	}
	h.showCustomScene(d, next, now, false)
	return nil
}

func (h *wsHub) localTap(d *wsDevice, id string, waiting bool, now time.Time) {
	controls := d.scene.controls[id]
	r, _ := controls["tap"].(*tapReaction)
	cycle, _ := controls["cycle"].(*reactionCycle)
	var step *tapReaction
	if cycle != nil {
		pos := d.cycles[id]
		if pos.cycle != cycle {
			pos = cyclePosition{cycle: cycle}
		}
		step = &(*cycle)[pos.index]
		pos.index = (pos.index + 1) % len(*cycle)
		if d.cycles == nil {
			d.cycles = make(map[string]cyclePosition)
		}
		d.cycles[id] = pos
	}
	if r != nil && h.react(d, *r, waiting, now) || step != nil && h.react(d, *step, waiting, now) {
		return
	}
	if controls["action"] == "exit" {
		h.leaveScene(d, waiting, now)
	}
}

func (h *wsHub) react(d *wsDevice, r tapReaction, waiting bool, now time.Time) bool {
	if r.chime != "" {
		h.chime(d, r.chime)
	}
	if r.state != "" {
		if view, ok := d.states[r.state]; ok {
			h.stopSceneTimers(d)
			d.cycles = nil
			h.showCustomScene(d, view, now, true)
		}
	}
	if r.patch != nil {
		if err := h.patchScene(d, r.patch, now); err != nil {
			h.push(d, object{"op": "error", "status": 400, "error": err.Error()}, true)
		}
	}
	if r.exit {
		h.leaveScene(d, waiting, now)
	}
	if r.ask != "" && !waiting {
		h.startTurn(d, now, true, func(ctx context.Context, id uint64) { h.runTurn(ctx, d, id, r.ask, nil) })
		if d.customScene {
			d.agentSetScene = true
		}
	}
	if r.speak != "" {
		h.speakLocal(d, r.speak)
	}
	if r.then != nil && !r.exit && d.customScene && len(d.sceneTimers) < maxSceneTimers {
		if d.sceneTimers == nil {
			d.sceneTimers = make(map[*time.Timer]struct{})
		}
		var timer *time.Timer
		timer = time.AfterFunc(r.delay, func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			// Membership identifies this scene generation even if Stop lost
			// the race with a callback waiting to acquire the hub lock.
			if _, ok := d.sceneTimers[timer]; ok {
				delete(d.sceneTimers, timer)
				if h.active(d) && d.customScene {
					h.react(d, *r.then, h.hasWaiter(d), time.Now())
				}
			}
		})
		d.sceneTimers[timer] = struct{}{}
	}
	return r.exit
}

func (h *wsHub) speakLocal(d *wsDevice, text string) {
	worker := h.b.worker
	if worker == nil || !worker.ready.Load() {
		return
	}
	voice := h.b.voiceFor(d.who)
	stream := newAudioStream()
	playback := h.play(d, d.client.ctx, stream)
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
