package main

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

const svgStart = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 360">`

const (
	menuBG    = "#151826"
	menuInk   = "#F2F4FB"
	menuMuted = "#BFC9DD"
)

var uiIcons = map[string]string{
	"menu":     "M3 5h18M3 12h18M3 19h18",
	"cancel":   "M5 5l14 14M19 5L5 19",
	"check":    "M4 12l5 5L20 6",
	"stop":     "M6 6h12v12H6z",
	"back":     "M11 5l-7 7 7 7M4 12h16",
	"next":     "M13 5l7 7-7 7M20 12H4",
	"intercom": "M6 7V2m-3 5h10v15H3zm3 5h4m-4 4h4m7-8q4 4 0 8m3-11q6 7 0 14",
	"volume":   "M3 9h4l6-5v16l-6-5H3zm13 0q3 3 0 6m3-9q5 6 0 12",
	"voice":    "M2 12h2l2-5 4 12 4-15 3 10 2-2h3",
	"stats":    "M2 5v15h20M5 14h4l3-7 3 9 3-5h4",
	"plus":     "M4 12h16M12 4v16",
	"minus":    "M4 12h16",
	"call":     "M5 4l4-1 3 5-3 3q2 4 6 6l3-3 5 3-1 4q-1 2-5 1Q6 19 3 8q-1-3 2-4z",
}

type bounds struct{ x, y, w, h int }
type palette struct{ bg, ink string }

var (
	navPalette    = palette{"#262E43", menuInk}
	dangerPalette = palette{"#8C3045", "#FFFFFF"}
	answerPalette = palette{"#54E6A9", "#0A2821"}
	volumePalette = palette{"#203144", "#A6DCFF"}
	voicePalette  = palette{"#322741", "#E2C5FF"}
)

type tile struct {
	id, label, detail, icon, badge string
	action                         sceneAction
	colors                         palette
}

func actionTile(id, label, icon string, colors palette) tile {
	return tile{id: id, label: label, icon: icon, action: sceneAction{id, ""}, colors: colors}
}

func (s *sceneView) control(b bounds, t tile) {
	x, y, w, h := b.x, b.y, b.w, b.h
	bg, fg, text, sub, icon := t.colors.bg, t.colors.ink, t.label, t.detail, t.icon
	r, tx, ty, size := h/2, x+w/2, y+h/2+6, 16
	ix, iy, is := x+w/2-10, y+h/2-10, 20
	if h == 78 {
		r, iy, ty, size = 18, y+13, y+57, 18
		if sub != "" {
			iy, ty = y+9, y+47
		}
		if t.badge != "" {
			size = 15
			text = truncate(text, 11)
		}
	} else if icon == "plus" || icon == "minus" {
		is, ix, iy = 30, x+w/2-15, y+h/2-15
	} else if icon != "" {
		size = 18
		ix, tx = x+w/2-(runeLen(text)*size/2+26)/2, tx+12
	}
	art := svgRect(x, y, w, h, r, bg, fg, 2)
	if path := uiIcons[icon]; path != "" {
		scale := float64(is) / 24
		art += fmt.Sprintf(`<path d="%s" transform="translate(%d %d) scale(%.4f %.4f)" fill="none" stroke="%s" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>`, path, ix, iy, scale, scale, fg)
	}
	if t.badge != "" {
		art += svgRect(tx-13, iy+2, 26, 13, 6, t.badge, fg, 1)
	}
	if icon != "plus" && icon != "minus" {
		art += svgLabel(tx, ty, size, text, fg)
	}
	if sub != "" {
		art += svgText(tx, y+66, 13, sub, menuMuted, "")
	}
	name := t.label
	switch icon {
	case "plus":
		name = "Raise volume"
	case "minus":
		name = "Lower volume"
	}
	s.button(t.id, name, t.action, x, y, w, h, art)
}

func (s *sceneView) stopControl() {
	s.control(bounds{120, 292, 120, 46}, actionTile("stop", "Stop", "stop", palette{"#32263C", "#FFE3E8"}))
}

func menuHeader(title string) sceneView {
	return sceneView{bg: menuBG, title: title, art: svgLabel(180, 76, 28, title, menuInk)}
}

func menuScene(title string) sceneView {
	s := menuHeader(title)
	s.control(bounds{120, 292, 120, 46}, actionTile("menu-back", "Back", "back", navPalette))
	return s
}

const tilesPerPage = 4

func clampTilePage(page, count int) int {
	return min(max(page, 0), (max(count, 1)-1)/tilesPerPage)
}

func tileGrid(title string, choices []tile, page int) sceneView {
	page = clampTilePage(page, len(choices))
	start, end := page*tilesPerPage, min(len(choices), (page+1)*tilesPerPage)
	s := menuHeader(title)
	for i, choice := range choices[start:end] {
		s.control(bounds{57 + 132*(i%2), 86 + 95*(i/2), 114, 78}, choice)
	}
	if len(choices) <= tilesPerPage {
		s.control(bounds{120, 292, 120, 46}, actionTile("menu-back", "Back", "back", navPalette))
	} else {
		if start > 0 {
			s.control(bounds{84, 276, 49, 42}, tile{id: "menu-prev", label: "Prev", action: sceneAction{"menu-page", "-1"}, colors: navPalette})
		}
		s.control(bounds{151, 276, 58, 42}, actionTile("menu-back", "Back", "", navPalette))
		if end < len(choices) {
			s.control(bounds{227, 276, 49, 42}, tile{id: "menu-next", label: "Next", action: sceneAction{"menu-page", "1"}, colors: navPalette})
		}
	}
	return s
}

func menuHome(voice string, auto bool, level int, scoped, calling bool, page int) sceneView {
	link := func(id, label, detail, icon string, colors palette) tile {
		return tile{id: "menu-" + id, label: label, detail: detail, icon: icon, colors: colors, action: sceneAction{"menu-open", id}}
	}
	choices := []tile{
		link("intercom", "Voice Note", "", "intercom", palette{"#372530", "#FFADAC"}),
		link("volume", "Volume", volumeLabel(auto, level), "volume", volumePalette),
		link("voice", "Voice", voiceName(voice), "voice", voicePalette),
	}
	if calling {
		choices = append(choices, link("call", "Call", "", "call", palette{"#193833", "#80F1C5"}))
	}
	if !scoped {
		choices = append(choices, link("stats", "Stats", "", "stats", palette{"#193833", "#80F1C5"}))
	}
	return tileGrid("Menu", choices, page)
}

func volumeScene(auto bool, level int) sceneView {
	s := menuScene("Volume")
	s.art += svgLabel(180, 125, 31, volumeLabel(auto, level), menuInk) + svgRect(71, 145, 218, 12, 6, "#323D52", "none", 0)
	if level > 0 {
		s.art += svgRect(71, 145, max(1, 218*level/100), 12, 6, "#7BD3FF", "none", 0)
	}
	s.control(bounds{91, 174, 68, 56}, actionTile("vol-minus", "−", "minus", volumePalette))
	s.control(bounds{201, 174, 68, 56}, actionTile("vol-plus", "+", "plus", volumePalette))
	icon, tint := "", menuMuted
	if auto {
		icon, tint = "check", "#A6DCFF"
	}
	s.control(bounds{116, 238, 128, 46}, actionTile("vol-auto", "Auto", icon, palette{volumePalette.bg, tint}))
	return s
}

func voiceScene(current string, page int) sceneView {
	choices := make([]tile, 0, len(voiceCatalog))
	for _, voice := range voiceCatalog {
		icon, tint := "voice", "#E2C5FF"
		if current == voice.ID {
			icon, tint = "check", "#FFD68B"
		}
		choices = append(choices, tile{id: "voice-" + strings.ToLower(voice.Name), label: voice.Name, icon: icon,
			action: sceneAction{"voice-select", voice.ID}, colors: palette{voicePalette.bg, tint}})
	}
	return tileGrid("Voice", choices, page)
}

type menuPeer struct {
	id, name, face, accent string
}

func peerScene(peers []menuPeer, page int, calling bool) sceneView {
	title, prefix, action := "Voice Note", "intercom", "reply"
	if calling {
		title, prefix, action = "Call", "call", "call-peer"
	}
	choices := make([]tile, 0, len(peers))
	for i, p := range peers {
		id := prefix + "-" + p.id
		if len(id) > 64 {
			id = prefix + ":" + strconv.Itoa(i)
		}
		choices = append(choices, tile{id: id, label: p.name, badge: p.accent,
			action: sceneAction{action, p.id}, colors: palette{p.face, menuPeerInk(p.face)}})
	}
	s := tileGrid(title, choices, page)
	if len(choices) == 0 {
		s.art += svgLabel(180, 167, 19, "No call-ready cats online", menuMuted)
	}
	return s
}

func callScene(state callState, id uint32, peer catState, muted bool, elapsed time.Duration) sceneView {
	name := truncate(peer.Name, 20)
	title := "In call · " + name
	switch state {
	case callRingingOut:
		title = "Calling " + name + "…"
	case callRingingIn:
		title = name + " calling"
	}
	ink := menuPeerInk(peer.FaceBG)
	s := sceneView{bg: menuBG, title: title, call: id,
		art: svgCircle(180, 113, 48, peer.FaceBG) + svgCircle(180, 86, 8, peer.AccentColor) +
			svgCircle(164, 107, 5, ink) + svgCircle(196, 107, 5, ink) +
			svgRect(175, 120, 10, 6, 3, ink, "none", 0) +
			svgLabel(180, 190, min(27, 440/max(1, runeLen(title))), title, menuInk)}
	left, right, center := bounds{68, 245, 100, 54}, bounds{192, 245, 100, 54}, bounds{115, 260, 130, 54}
	if state == callRingingIn {
		s.control(left, actionTile("call-answer", "Answer", "", answerPalette))
		s.control(right, actionTile("call-decline", "Decline", "", dangerPalette))
	} else if state == callActive {
		s.art += svgLabel(180, 229, 24, callDuration(elapsed), menuMuted)
		label := "Mute"
		if muted {
			label = "Unmute"
		}
		s.control(left, actionTile("call-mute", label, "", navPalette))
		s.control(right, actionTile("call-hangup", "Hang Up", "", dangerPalette))
	} else {
		s.control(center, actionTile("call-hangup", "Hang Up", "", dangerPalette))
	}
	return s
}

// Pick black or white to retain at least 4.5:1 contrast on a custom cat color.
func menuPeerInk(tint string) string {
	n, _ := strconv.ParseUint(strings.TrimPrefix(tint, "#"), 16, 32)
	channel := func(shift int) float64 {
		v := float64(n>>shift&255) / 255
		if v <= .04045 {
			return v / 12.92
		}
		return math.Pow((v+.055)/1.055, 2.4)
	}
	if .2126*channel(16)+.7152*channel(8)+.0722*channel(0) > .179 {
		return "#000000"
	}
	return "#FFFFFF"
}

func (h *wsHub) statsScene(d *wsDevice) sceneView {
	s := menuScene("Stats")
	s.art += svgRect(38, 93, 284, 185, 18, "#0E111B", "none", 0)
	device := d.diagDevice()
	h.b.diag.mu.Lock()
	sys := h.b.diag.system
	latest := [4]*float64{}
	for i, metric := range []diagMetric{diagSTT, diagLLMFirst, diagTTSFirst, diagRender} {
		if r := h.b.diag.metrics[metric]; len(r.items) != 0 {
			latest[i] = new(r.last())
		}
	}
	h.b.diag.mu.Unlock()
	value := func(n *float64, suffix string) string {
		if n == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f%s", *n, suffix)
	}
	bytes := func(n *uint64, unit uint64, suffix string) string {
		if n == nil {
			return "—"
		}
		return fmt.Sprintf("%d%s", *n/unit, suffix)
	}
	integer := func(n *int, suffix string) string {
		if n == nil {
			return "—"
		}
		return fmt.Sprint(*n) + suffix
	}
	battery := "Bat " + integer(device.Battery, "%")
	if device.Kind == "virtual" {
		battery = "Web"
	} else if !d.caps.Battery {
		battery = "USB"
	}
	mode := "Man"
	if d.autoVol {
		mode = "Auto"
	}
	if d.frame != nil {
		latest[3] = &d.frame.MS
	}
	fw, board := device.Firmware, device.Board
	if fw == "" {
		fw = "—"
	}
	if board == "" {
		board = device.Kind
	}
	rows := []string{
		fmt.Sprintf("%s · %s · %s", truncate(device.Character, 15), truncate(fw, 12), board),
		fmt.Sprintf("CPU %s · WiFi %s · RTT %s", value(device.CPU, "%"), integer(device.WiFi, "dBm"), value(device.RTT, "ms")),
		fmt.Sprintf("SRAM %s · PSRAM %s · %s", bytes(device.FreeHeap, 1024, "K"), bytes(device.FreePSRAM, 1<<20, "M"), battery),
		fmt.Sprintf("Vol %d%% (%s) · Amb %s", d.vol, mode, value(device.Ambient, "")),
		fmt.Sprintf("STT %s · LLM %s · TTS %s · Frame %s", value(latest[0], ""), value(latest[1], ""), value(latest[2], ""), value(latest[3], "")),
		fmt.Sprintf("Go %s %s · TTS %s · Up %s", value(sys.Bridge.CPU, "%"), bytes(sys.Bridge.RSS, 1<<20, "M"),
			bytes(sys.Worker.RSS, 1<<20, "M"), (time.Duration(sys.Bridge.Uptime) * time.Second).Truncate(time.Second).String()),
	}
	for i, row := range rows {
		tint := menuInk
		if i == 0 {
			tint = "#80F1C5"
		}
		s.art += svgText(180, 121+i*27, min(15, 438/max(1, runeLen(row))), row, tint, `font-family="monospace"`)
	}
	return s
}

func defaultFaceSVG(faceBG, markColor, accentColor string, configured bool) string {
	mark := `<circle cx="167" cy="79" r="3"/><circle cx="180" cy="79" r="3"/><circle cx="193" cy="79" r="3"/>`
	if configured {
		mark = `<rect x="147" y="66" width="14" height="24" rx="7"/>
    <rect x="173" y="61" width="14" height="33" rx="7"/>
    <rect x="199" y="66" width="14" height="24" rx="7"/>`
	}
	const smile = `<path d="M144 198 Q160 225 180 201 Q200 225 216 198" fill="none" stroke="#1A1926" stroke-width="7" stroke-linecap="round"/>`
	return fmt.Sprintf(svgStart+`
  <rect width="360" height="360" fill="%s"/>
  <g fill="%s">
    %s
  </g>
  <path d="M38 177L87 188 M41 212L87 199 M273 188L322 177 M273 199L319 212" fill="none" stroke="#1A1926" stroke-width="6" stroke-linecap="round"/>
  <g fill="%s" opacity="0.48">
    <ellipse cx="103" cy="211" rx="14" ry="8"/>
    <ellipse cx="257" cy="211" rx="14" ry="8"/>
  </g>
  <rect x="165" y="163" width="30" height="17" rx="8" fill="#1A1926"/>
  <g data-pose="idle">
    <rect x="95" y="123" width="34" height="38" rx="17" fill="#1A1926"/>
    <rect x="231" y="123" width="34" height="38" rx="17" fill="#1A1926"/>
    <circle cx="107" cy="134" r="5" fill="#FFFAEE"/>
    <circle cx="243" cy="134" r="5" fill="#FFFAEE"/>
    %s
  </g>
  <g data-pose="blink">
    <path d="M95 142H129 M231 142H265" fill="none" stroke="#1A1926" stroke-width="8" stroke-linecap="round"/>
    %s
  </g>
  <g data-pose="talk">
    <path d="M93 149Q112 119 131 149 M229 149Q248 119 267 149" fill="none" stroke="#1A1926" stroke-width="7" stroke-linecap="round"/>
    <rect x="143" y="190" width="74" height="39" rx="19" fill="#1A1926"/>
    <ellipse cx="180" cy="220" rx="15" ry="5" fill="#F47E92"/>
  </g>
  <g data-pose="listen">
    <ellipse cx="112" cy="142" rx="20" ry="22" fill="#1A1926"/>
    <ellipse cx="248" cy="142" rx="20" ry="22" fill="#1A1926"/>
    <circle cx="107" cy="133" r="5" fill="#FFFAEE"/>
    <circle cx="243" cy="133" r="5" fill="#FFFAEE"/>
    <circle cx="180" cy="205" r="12" fill="#1A1926"/>
  </g>
  <g data-pose="think">
    <rect x="95" y="123" width="34" height="38" rx="17" fill="#1A1926"/>
    <circle cx="107" cy="134" r="5" fill="#FFFAEE"/>
    <path d="M231 144H265 M168 205L192 200" fill="none" stroke="#1A1926" stroke-width="7" stroke-linecap="round"/>
    <g fill="#1A1926">
      <circle cx="163" cy="252" r="4"/><circle cx="180" cy="245" r="4"/><circle cx="197" cy="252" r="4"/>
    </g>
  </g>
  <g data-pose="sleep">
    <path d="M92 139Q112 166 132 139 M228 139Q248 166 268 139" fill="none" stroke="#1A1926" stroke-width="7" stroke-linecap="round"/>
    %s
    <text x="215" y="113" fill="#1A1926" font-weight="bold" font-size="16">z</text>
    <text x="233" y="100" fill="#1A1926" font-weight="bold" font-size="24">Z</text>
  </g>
</svg>`, html.EscapeString(faceBG), html.EscapeString(markColor), mark, html.EscapeString(accentColor), smile, smile, smile)
}

func parseFaceSVG(raw string) (*svgNode, error) {
	if len(raw) > 16<<10 {
		return nil, fmt.Errorf("face_svg exceeds 16 KB")
	}
	root, err := parseSVG(raw)
	if err != nil {
		return nil, err
	}
	var stripControls func(*svgNode)
	stripControls = func(n *svgNode) {
		delete(n.attr, "data-id")
		delete(n.attr, "data-hit")
		for _, child := range n.children {
			stripControls(child)
		}
	}
	stripControls(root)
	return root, nil
}

func sanitizeFaceSVG(raw string) (string, error) {
	root, err := parseFaceSVG(raw)
	if err != nil {
		return "", problem(400, err.Error())
	}
	clean := encodeSVG(root, "  ")
	if len(clean) > 16<<10 {
		clean = svgString(root)
	}
	if len(clean) > 16<<10 {
		return "", problem(400, "face_svg exceeds 16 KB after sanitizing")
	}
	// Validate all poses together with chrome, including the compositor's depth
	// and node overhead, so no accepted face can crowd out the system controls.
	chrome := catChrome(blankCat(), catPose{mood: "listening", battery: 100, charge: true, meter: true})
	if _, _, err := renderSVG(chrome.svg(svgString(root.asGroup()))); err != nil {
		return "", problem(400, err.Error())
	}
	return clean, nil
}

func recolorFaceSVG(raw string, old, next [3]string) (string, error) {
	root, err := parseFaceSVG(raw)
	if err != nil {
		return "", problem(400, err.Error())
	}
	var recolor func(*svgNode)
	recolor = func(n *svgNode) {
		for _, key := range []string{"fill", "stroke", "stop-color"} {
			for i, tint := range old {
				if tint != next[i] && strings.EqualFold(n.attr[key], tint) {
					n.attr[key] = next[i]
					break
				}
			}
		}
		for _, child := range n.children {
			recolor(child)
		}
	}
	recolor(root)
	return sanitizeFaceSVG(svgString(root))
}

func (p catPose) facePose() string {
	switch p.mood {
	case "sleepy":
		return "sleep"
	case "listening":
		return "listen"
	case "thinking":
		return "think"
	case "speaking":
		return "talk"
	default:
		if p.blink {
			return "blink"
		}
		return "idle"
	}
}

func faceLayer(root *svgNode, pose string) *svgNode {
	// A nested pose is only available if all its ancestors would be kept.
	var hasPose func(*svgNode) bool
	hasPose = func(n *svgNode) bool {
		if p, set := n.attr["data-pose"]; set {
			return p == pose
		}
		for _, child := range n.children {
			if hasPose(child) {
				return true
			}
		}
		return false
	}
	if !hasPose(root) {
		pose = "idle"
	}
	var filter func(*svgNode) *svgNode
	filter = func(n *svgNode) *svgNode {
		if p, set := n.attr["data-pose"]; set && p != pose {
			return nil
		}
		children := n.children[:0]
		for _, child := range n.children {
			if kept := filter(child); kept != nil {
				children = append(children, kept)
			}
		}
		n.children = children
		return n
	}
	if root = filter(root); root != nil {
		return root.asGroup()
	}
	return root
}

func catSVG(cat catState, pose catPose) string {
	return catChrome(cat, pose).svg(catArt(cat, pose))
}

type faceLayerKey struct {
	hash [32]byte
	pose string
}

const faceLayerCacheSize = 64

type faceLayerCache struct {
	mu      sync.Mutex
	entries map[faceLayerKey]string
	order   [faceLayerCacheSize]faceLayerKey
	next    int
}

var faceLayers faceLayerCache

func (c *faceLayerCache) layer(raw, pose string) (string, error) {
	key := faceLayerKey{svgHash(raw), pose}
	c.mu.Lock()
	if layer, ok := c.entries[key]; ok {
		c.mu.Unlock()
		return layer, nil
	}
	c.mu.Unlock()

	root, err := parseFaceSVG(raw)
	if err != nil {
		return "", err
	}
	var art string
	if n := faceLayer(root, pose); n != nil {
		art = svgString(n)
	}
	c.mu.Lock()
	if _, ok := c.entries[key]; !ok {
		if c.entries == nil {
			c.entries = make(map[faceLayerKey]string, faceLayerCacheSize)
		}
		delete(c.entries, c.order[c.next])
		c.order[c.next] = key
		c.next = (c.next + 1) % len(c.order)
		c.entries[key] = art
	}
	c.mu.Unlock()
	return art, nil
}

func catArt(cat catState, pose catPose) string {
	p := pose.facePose()
	art, err := faceLayers.layer(cat.FaceSVG, p)
	if err != nil {
		art, _ = faceLayers.layer(cat.defaultFace(), p)
	}
	return art
}
