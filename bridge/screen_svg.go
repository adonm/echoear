package main

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
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
}

func (s *sceneView) control(id, action, value string, x, y, w, h int, text, sub, icon, bg, fg string) {
	r, tx, ty, size := h/2, x+w/2, y+h/2+6, 16
	ix, iy, is := x+w/2-10, y+h/2-10, 20
	if h == 78 {
		r, iy, ty, size = 18, y+13, y+57, 18
		if sub != "" {
			iy, ty = y+9, y+47
		}
	} else if icon == "plus" || icon == "minus" {
		is, ix, iy = 30, x+w/2-15, y+h/2-15
	} else if icon != "" {
		size = 18
		ix, tx = x+w/2-(runeLen(text)*size/2+26)/2, tx+12
	}
	art := svgRect(x, y, w, h, r, bg, fg, 2)
	if path := uiIcons[icon]; path != "" {
		art += fmt.Sprintf(`<path d="%s" transform="translate(%d %d) scale(%.4f)" fill="none" stroke="%s" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>`, path, ix, iy, float64(is)/24, fg)
	}
	if icon != "plus" && icon != "minus" {
		art += svgLabel(tx, ty, size, text, fg)
	}
	if sub != "" {
		art += svgText(tx, y+66, 13, sub, menuMuted, "")
	}
	name := text
	switch icon {
	case "plus":
		name = "Raise volume"
	case "minus":
		name = "Lower volume"
	}
	s.button(id, name, sceneAction{action, value}, x, y, w, h, art)
}

func (s *sceneView) stopControl() {
	s.control("stop", "stop", "", 120, 292, 120, 46, "Stop", "", "stop", "#32263C", "#FFE3E8")
}

func menuScene(title string) sceneView {
	s := sceneView{bg: menuBG, title: title, art: svgLabel(180, 76, 28, title, menuInk)}
	s.control("menu-back", "menu-back", "", 120, 292, 120, 46, "Back", "", "back", "#262E43", menuInk)
	return s
}

func (s *sceneView) menuTile(i int, id, action, value, label, sub, icon, bg, fg string) {
	s.control(id, action, value, 58+130*(i%2), 93+88*(i/2), 114, 78, label, sub, icon, bg, fg)
}

func menuHome(voice string, auto bool, level int, scoped bool) sceneView {
	s := menuScene("Menu")
	s.menuTile(0, "menu-intercom", "menu-open", "intercom", "Talk to…", "", "intercom", "#372530", "#FFADAC")
	s.menuTile(1, "menu-volume", "menu-open", "volume", "Volume", volumeLabel(auto, level), "volume", "#203144", "#A6DCFF")
	s.menuTile(2, "menu-voice", "menu-open", "voice", "Voice", voiceName(voice), "voice", "#322741", "#E2C5FF")
	if !scoped {
		s.menuTile(3, "menu-stats", "menu-open", "stats", "Stats", "", "stats", "#193833", "#80F1C5")
	}
	return s
}

func volumeScene(auto bool, level int) sceneView {
	s := menuScene("Volume")
	s.art += svgLabel(180, 125, 31, volumeLabel(auto, level), menuInk) + svgRect(71, 145, 218, 12, 6, "#323D52", "none", 0)
	if level > 0 {
		s.art += svgRect(71, 145, max(1, 218*level/100), 12, 6, "#7BD3FF", "none", 0)
	}
	s.control("vol-minus", "vol-minus", "", 91, 174, 68, 56, "−", "", "minus", "#203144", "#A6DCFF")
	s.control("vol-plus", "vol-plus", "", 201, 174, 68, 56, "+", "", "plus", "#203144", "#A6DCFF")
	icon, tint := "", menuMuted
	if auto {
		icon, tint = "check", "#A6DCFF"
	}
	s.control("vol-auto", "vol-auto", "", 116, 238, 128, 46, "Auto", "", icon, "#203144", tint)
	return s
}

func voiceScene(current string) sceneView {
	s := menuScene("Voice")
	for i, voice := range voiceCatalog[:4] {
		icon, tint := "voice", "#E2C5FF"
		if current == voice.ID {
			icon, tint = "check", "#FFD68B"
		}
		s.menuTile(i, "voice-"+strings.ToLower(voice.Name), "voice-select", voice.ID, voice.Name, "", icon, "#322741", tint)
	}
	return s
}

type menuPeer struct {
	id, name, face, accent string
}

func intercomSceneMenu(peers []menuPeer, page int) sceneView {
	s := menuScene("Talk to…")
	pageSize := 4
	start, end := page*pageSize, min(len(peers), (page+1)*pageSize)
	for i, p := range peers[start:end] {
		x, y := 58+130*(i%2), 86+72*(i/2)
		id := "intercom-" + p.id
		if len(id) > 64 {
			id = "intercom:" + strconv.Itoa(start+i)
		}
		fg := menuPeerInk(p.face)
		s.button(id, p.name, sceneAction{"reply", p.id}, x, y, 114, 62,
			svgRect(x, y, 114, 62, 31, p.face, menuMuted, 2)+
				svgRect(x+46, y+11, 22, 9, 4, p.accent, fg, 1)+
				svgLabel(x+57, y+46, 14, truncate(p.name, 11), fg))
	}
	if start > 0 {
		s.control("intercom-prev", "intercom-page", "-1", 85, 231, 84, 46, "Prev", "", "", "#262E43", menuInk)
	}
	if end < len(peers) {
		s.control("intercom-next", "intercom-page", "1", 191, 231, 84, 46, "Next", "", "", "#262E43", menuInk)
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
	if device.HasBattery != nil && !*device.HasBattery {
		battery = "USB"
	} else if device.Kind == "virtual" {
		battery = "Web"
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
	if _, _, err := renderSVG(chrome.svg(svgString(wrapFaceLayer(root)))); err != nil {
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
		return wrapFaceLayer(root)
	}
	return root
}

func wrapFaceLayer(root *svgNode) *svgNode {
	root.tag = "g"
	delete(root.attr, "viewBox")
	delete(root.attr, "xmlns")
	return root
}

func catSVG(cat catState, pose catPose) string {
	return catChrome(cat, pose).svg(catArt(cat, pose))
}

func catArt(cat catState, pose catPose) string {
	root, err := parseFaceSVG(cat.FaceSVG)
	if err != nil {
		root, _ = parseFaceSVG(cat.defaultFace())
	}
	if layer := faceLayer(root, pose.facePose()); layer != nil {
		return svgString(layer)
	}
	return ""
}
