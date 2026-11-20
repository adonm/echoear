package main

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

const catInk = "#1A1926"

type palette struct{ face, mark, accent string }

var catPalettes = map[string]palette{
	"cake":  {"#FFF3D1", "#F59E0B", "#FACC15"},
	"mint":  {"#7BE3C6", "#228E81", "#FFD166"},
	"plum":  {"#D8C7FF", "#9F7AEA", "#DCB5FF"},
	"sunny": {"#FCD34D", "#EA580C", "#FFBB6E"},
}

func catPalette(character string) palette {
	if p, ok := catPalettes[strings.ToLower(character)]; ok {
		return p
	}
	return catPalettes["cake"]
}

type sceneAction struct{ kind, value string }

type sceneView struct {
	bg       string
	nodes    []object
	actions  map[string]sceneAction
	feedback map[string]string
	controls map[string]object // Server-only node declarations.
}

func (s sceneView) full(rev uint64) object {
	return object{"op": "scene", "rev": rev, "bg": s.bg, "nodes": s.nodes}
}

// A patch retains the order of surviving nodes and can only append new ones.
func sceneDiff(old, next sceneView, rev uint64) object {
	before := make(map[string]object, len(old.nodes))
	after := make(map[string]object, len(next.nodes))
	for _, node := range old.nodes {
		before[str(node["id"])] = node
	}
	retained, appended := make([]string, 0, len(next.nodes)), make([]string, 0)
	add, gone := []object{}, []string{}
	set := map[string]object{}
	for _, node := range next.nodes {
		id := str(node["id"])
		after[id] = node
		prev := before[id]
		if prev == nil {
			appended, add = append(appended, id), append(add, node)
			continue
		}
		retained = append(retained, id)
		if prev["t"] != node["t"] {
			return next.full(rev)
		}
		for k := range prev {
			if _, ok := node[k]; !ok {
				return next.full(rev)
			}
		}
		if !maps.Equal(prev, node) {
			fields := object{}
			for k, v := range node {
				if prev[k] != v {
					fields[k] = v
				}
			}
			set[id] = fields
		}
	}
	var index int
	for _, node := range old.nodes {
		id := str(node["id"])
		if after[id] == nil {
			gone = append(gone, id)
		} else {
			if id != retained[index] {
				return next.full(rev)
			}
			index++
		}
	}
	for i, id := range append(retained, appended...) {
		if next.nodes[i]["id"] != id {
			return next.full(rev)
		}
	}
	if old.bg == next.bg && len(set)+len(add)+len(gone) == 0 {
		return nil
	}
	out := object{"op": "patch", "rev": rev, "set": set, "add": add, "del": gone}
	if old.bg != next.bg {
		out["bg"] = next.bg
	}
	return out
}

func box(id string, x, y, w, h, r int, tint string) object {
	return object{"id": id, "t": "rect", "x": x, "y": y, "w": w, "h": h, "r": r, "c": tint, "fill": true}
}

func circ(id string, x, y, r int, tint string) object {
	return object{"id": id, "t": "circle", "x": x, "y": y, "r": r, "c": tint, "fill": true}
}

func segment(id string, x1, y1, x2, y2, width int, tint string) object {
	return object{"id": id, "t": "line", "x1": x1, "y1": y1, "x2": x2, "y2": y2, "sw": width, "c": tint}
}

func arc(id string, x, y, r, a1, a2, width int, tint string) object {
	return object{"id": id, "t": "arc", "x": x, "y": y, "r": r, "a1": a1, "a2": a2, "sw": width, "c": tint}
}

func label(id string, x, y, scale int, text, tint string) object {
	return object{"id": id, "t": "text", "x": x, "y": y, "s": text, "c": tint, "fs": scale}
}

func comicButton(id string, x, y, w, h int, text, tint string) object {
	return object{"id": id, "t": "btn", "x": x, "y": y, "w": w, "h": h, "r": h / 2, "c": tint,
		"bc": catInk, "tc": catInk, "sw": 3, "fill": true, "s": text, "fs": 1, "on": "tap"}
}

func visible(n object, yes bool) object    { n["vis"] = yes; return n }
func bordered(n object, stroke int) object { n["bc"], n["sw"] = catInk, stroke; return n }

type catPose struct {
	mood    string
	blink   bool
	mouth   bool
	dot     int
	battery int
	charge  bool
	volume  int
	meter   bool
}

func catScene(character string, pose catPose) sceneView {
	p := catPalette(character)
	listening, thinking := pose.mood == "listening", pose.mood == "thinking"
	speaking, sleeping := pose.mood == "speaking", pose.mood == "sleepy"
	s := sceneView{bg: p.face, nodes: make([]object, 0, 40), actions: map[string]sceneAction{
		"text": {"drawer", ""}, "cat-ring": {"listen", ""}, "volume-track": {"volume", ""},
	}}
	add := func(ns ...object) { s.nodes = append(s.nodes, ns...) }
	ringColor, ringWidth := catInk, 3
	switch pose.mood {
	case "listening":
		ringColor, ringWidth = "#FF5964", 5
	case "thinking":
		ringColor, ringWidth = "#F59E0B", 5
	case "speaking":
		ringColor, ringWidth = "#0284C7", 5
	}
	ring := circ("cat-ring", 180, 180, 172, ringColor)
	ring["fill"], ring["sw"], ring["on"] = false, ringWidth, "tap"
	add(ring)
	switch strings.ToLower(character) {
	case "mint":
		add(box("stripe-mid", 156, 72, 48, 12, 6, p.mark))
	case "plum":
		add(circ("stripe-mid", 180, 79, 18, p.mark), circ("stripe-cut", 191, 71, 16, p.face))
	case "sunny":
		add(circ("stripe-mid", 180, 78, 16, p.mark))
	default:
		add(box("stripe-left", 147, 66, 14, 24, 7, p.mark), box("stripe-mid", 173, 61, 14, 33, 7, p.mark),
			box("stripe-right", 199, 66, 14, 24, 7, p.mark))
	}
	for i, x := range []int{112, 248} {
		tag := []string{"-l", "-r"}[i]
		shut := pose.blink && !thinking || thinking && i == 1
		w, h := 34, 38
		if listening {
			w, h = 40, 44
		}
		add(visible(box("eye"+tag, x-w/2, 142-h/2, w, h, w/2, catInk), !shut && !speaking && !sleeping),
			visible(box("lid"+tag, x-17, 138, 34, 8, 4, catInk), shut && !speaking && !sleeping),
			visible(arc("eye-arc"+tag, x, 145, 22, 205, 335, 7, catInk), speaking),
			visible(arc("eye-sleep"+tag, x, 138, 21, 0, 180, 7, catInk), sleeping))
	}
	add(segment("whisker-l1", 38, 177, 87, 188, 6, catInk), segment("whisker-l2", 41, 212, 87, 199, 6, catInk),
		segment("whisker-r1", 273, 188, 322, 177, 6, catInk), segment("whisker-r2", 273, 199, 319, 212, 6, catInk),
		box("nose", 165, 163, 30, 17, 8, catInk))
	talk := speaking && pose.mouth
	smile := !talk && !listening && !thinking
	add(visible(arc("smile-l", 163, 193, 21, 0, 165, 7, catInk), smile),
		visible(arc("smile-r", 197, 193, 21, 15, 180, 7, catInk), smile),
		visible(circ("listen-mouth", 180, 208, 17, catInk), listening),
		visible(segment("think-mouth", 166, 204, 194, 200, 6, catInk), thinking),
		visible(box("talk-mouth", 140, 189, 80, 40, 20, catInk), talk),
		visible(box("talk-top", 140, 189, 80, 20, 5, catInk), talk),
		visible(box("tongue", 164, 214, 32, 9, 5, "#F47E92"), talk))
	for i := range 3 {
		y := 256
		if thinking && i == pose.dot {
			y -= 9
		}
		add(visible(circ("dot-"+strconv.Itoa(i), 163+i*17, y, 4, catInk), thinking))
	}
	add(visible(label("doze-small", 211, 109, 1, "z", catInk), sleeping),
		visible(label("doze-large", 230, 88, 2, "Z", catInk), sleeping))
	batteryColor := "#39D1A0"
	switch {
	case !pose.charge && pose.battery < 10:
		batteryColor = "#FF5964"
	case !pose.charge && pose.battery < 20:
		batteryColor = "#FACC15"
	}
	hasBattery := pose.battery >= 0
	add(visible(box("battery-tip", 258, 46, 6, 7, 2, catInk), hasBattery),
		visible(bordered(box("battery", 229, 41, 31, 17, 5, "#FFFAEE"), 2), hasBattery),
		visible(box("battery-fill", 232, 44, max(1, pose.battery*25/100), 11, 2, batteryColor), pose.battery > 0),
		visible(label("battery-charge", 246, 49, 1, "+", catInk), hasBattery && pose.charge))
	meter := bordered(box("volume-track", 114, 246, 132, 33, 16, "#FFFAEE"), 2)
	meter["on"] = "drag"
	add(visible(meter, pose.meter), visible(box("volume-fill", 123, 255, max(1, pose.volume*114/100), 15, 7, p.accent), pose.meter),
		visible(label("volume-label", 180, 263, 1, strconv.Itoa(pose.volume)+"%", catInk), pose.meter))
	add(visible(comicButton("text", 137, 284, 86, 36, "Text", "#FFFAEE"), !sleeping))
	return s
}

func drawerScene(character, voice string, peers []peer) sceneView {
	p := catPalette(character)
	s := sceneView{bg: "#251C3B", nodes: []object{
		label("drawer-title", 180, 57, 2, "Text", "#FFFAEE"),
		comicButton("cat", 84, 86, 91, 36, "Cat", p.face), comicButton("new", 185, 86, 91, 36, "New", p.accent),
		comicButton("voice", 90, 300, 180, 36, "Voice: "+voiceNames[voice], p.accent),
	}, actions: map[string]sceneAction{"cat": {"cat", ""}, "new": {"new", ""}, "voice": {"voice", ""}}}
	for i, who := range peers[:min(4, len(peers))] {
		id := "msg-" + strconv.Itoa(i)
		s.nodes = append(s.nodes, comicButton(id, 70, 133+i*41, 220, 35, "Msg "+truncate(who.Character, 22), p.face))
		s.actions[id] = sceneAction{"reply", who.DeviceID}
	}
	if len(peers) == 0 {
		s.nodes = append(s.nodes, label("peers-empty", 180, 176, 1, "No nearby cats", "#FFFAEE"))
	}
	return s
}

func screenScene(screen object, accent string) sceneView {
	bg := str(screen["bg"])
	if bg == "" {
		bg = "#0B1020"
	}
	s := sceneView{bg: bg, nodes: make([]object, 0, 29),
		actions: map[string]sceneAction{"cat": {"cat", ""}}, feedback: make(map[string]string)}
	if title := str(screen["title"]); title != "" {
		s.nodes = append(s.nodes, label("title", 180, 37, 2, title, "#FFF1C8"))
	}
	if subtitle := str(screen["subtitle"]); subtitle != "" {
		s.nodes = append(s.nodes, label("subtitle", 180, 62, 1, subtitle, "#FFF1C8"))
	} else {
		s.nodes = append(s.nodes, visible(label("subtitle", 180, 62, 1, "", "#FFF1C8"), false))
	}
	shapes, _ := screen["shapes"].([]object)
	for i, shape := range shapes[:min(24, len(shapes))] {
		id := "shape-" + strconv.Itoa(i)
		n := object{"id": id, "t": shape["t"], "c": shape["c"]}
		for _, key := range []string{"x", "y", "x1", "y1", "x2", "y2", "h", "r", "a1", "a2", "s", "fill"} {
			if v, ok := shape[key]; ok {
				n[key] = v
			}
		}
		switch str(n["t"]) {
		case "rect", "btn":
			n["w"] = shape["w"]
			if n["t"] == "btn" {
				n["bc"], n["tc"], n["sw"], n["fill"], n["fs"] = catInk, contrastText(str(n["c"])), 3, true, 1
			}
		case "circle", "line", "arc":
			width := nodeInt(shape, "w")
			n["sw"] = max(1, width)
			if width == 0 {
				n["sw"] = 3
			}
		case "text":
			n["fs"] = 1
		}
		for _, key := range []string{"reply_to", "ask", "tap"} {
			if v := str(shape[key]); v != "" {
				if key == "reply_to" && !targetRE.MatchString(v) {
					continue
				}
				s.actions[id] = sceneAction{key, v}
				n["on"] = "tap"
				break
			}
		}
		if v := str(shape["tap"]); v != "" {
			s.feedback[id] = v
		}
		s.nodes = append(s.nodes, n)
	}
	s.nodes = append(s.nodes, comicButton("cat", 139, 301, 82, 32, "Cat", accent))
	return s
}

func (s sceneView) hint(message string) *sceneView {
	s.nodes = slices.Clone(s.nodes)
	for i, n := range s.nodes {
		if n["id"] == "subtitle" {
			s.nodes[i] = visible(label("subtitle", 180, 62, 1, truncate(message, 37), "#FFF1C8"), true)
			break
		}
	}
	return &s
}

func contrastText(tint string) string {
	if len(tint) == 7 {
		n, _ := strconv.ParseUint(tint[1:], 16, 32)
		if 299*(n>>16)+587*((n>>8)&255)+114*(n&255) < 140000 {
			return "#FFFAEE"
		}
	}
	return catInk
}

func intercomScene(sender peer, text string) sceneView {
	p := catPalette(sender.Character)
	if text == "" {
		text = "Voice message"
	}
	s := sceneView{bg: "#251C3B", nodes: []object{
		bordered(box("card", 42, 84, 276, 204, 29, "#FFFAEE"), 4),
		circ("badge", 180, 83, 17, p.accent),
		label("from", 180, 120, 2, truncate("From "+sender.Character+"!", 25), catInk),
	}, actions: map[string]sceneAction{"reply": {"reply", sender.DeviceID}, "cat": {"cat", ""}}}
	for i, line := range cardLines(text) {
		s.nodes = append(s.nodes, label("message-"+strconv.Itoa(i), 180, 155+i*23, 1, line, catInk))
	}
	s.nodes = append(s.nodes, comicButton("reply", 83, 226, 194, 38, "Reply to "+truncate(sender.Character, 18), p.accent),
		comicButton("cat", 139, 300, 82, 32, "Cat", p.face))
	return s
}

func noticeScene(title, message string) sceneView {
	s := intercomScene(peer{Character: "Cake"}, message)
	s.nodes[2]["s"] = title
	s.nodes = slices.Delete(s.nodes, len(s.nodes)-2, len(s.nodes)-1)
	delete(s.actions, "reply")
	return s
}

func nodeInt(n object, key string) int { v, _ := n[key].(int); return v }

func hitNode(n object, x, y int) bool {
	nx, ny := nodeInt(n, "x"), nodeInt(n, "y")
	dx, dy := float64(x-nx), float64(y-ny)
	switch n["t"] {
	case "circle", "arc":
		r := float64(nodeInt(n, "r"))
		return dx*dx+dy*dy <= r*r
	case "rect", "btn":
		return x >= nx && y >= ny && x <= nx+nodeInt(n, "w") && y <= ny+nodeInt(n, "h")
	case "text":
		return math.Abs(dy) <= 12 && math.Abs(dx) <= float64(max(14, 4*runeLen(str(n["s"]))))
	case "line":
		ax, ay, bx, by := float64(nodeInt(n, "x1")), float64(nodeInt(n, "y1")), float64(nodeInt(n, "x2")), float64(nodeInt(n, "y2"))
		vx, vy := bx-ax, by-ay
		at := clampFloat(((float64(x)-ax)*vx+(float64(y)-ay)*vy)/max(1, vx*vx+vy*vy), 0, 1)
		return math.Hypot(float64(x)-ax-at*vx, float64(y)-ay-at*vy) <= float64(max(7, nodeInt(n, "sw")))
	}
	return false
}

func clampFloat(v, lo, hi float64) float64 { return min(hi, max(lo, v)) }

func (s sceneView) action(id string, x, y int, on string) (string, sceneAction) {
	for _, n := range slices.Backward(s.nodes) {
		key := str(n["id"])
		if n["vis"] == false || n["on"] != on || id != "" && key != id || id == "" && !hitNode(n, x, y) {
			continue
		}
		if a, ok := s.actions[key]; ok {
			return key, a
		}
	}
	return "", sceneAction{}
}

func poseFrame(now, since, idle time.Time, mood string, pct, volume int, charge, meter bool) catPose {
	p := catPose{mood: mood, battery: pct, charge: charge, volume: volume, meter: meter}
	switch mood {
	case "idle", "listening":
		p.blink = now.Sub(idle)%(3500*time.Millisecond) >= 3350*time.Millisecond
	case "thinking":
		p.dot = int(now.Sub(since)/(160*time.Millisecond)) % 3
	case "speaking":
		p.mouth = now.Sub(since)%(320*time.Millisecond) < 160*time.Millisecond
	}
	return p
}
