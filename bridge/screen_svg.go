package main

import (
	"fmt"
	"html"
	"log"
	"math"
	"strings"
)

const svgStart = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 360">`

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
		delete(n.attr, "data-on")
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
	chrome := catChrome(blankCat(), catPose{battery: 100, charge: true, meter: true})
	if _, _, err := renderSVG(sceneSVG(chrome, svgString(wrapFaceLayer(root)))); err != nil {
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
	root, err := parseFaceSVG(cat.FaceSVG)
	if err != nil {
		root, _ = parseFaceSVG(cat.defaultFace())
	}
	return composedCatSVG(catChrome(cat, pose), root, pose.facePose())
}

func composedCatSVG(chrome sceneView, root *svgNode, pose string) string {
	art := ""
	if layer := faceLayer(root, pose); layer != nil {
		art = svgString(layer)
	}
	return sceneSVG(chrome, art)
}

func sceneSVG(s sceneView, art string) string {
	var out strings.Builder
	out.WriteString(svgStart)
	fmt.Fprintf(&out, `<rect width="360" height="360" fill="%s"/>`, s.bg)
	out.WriteString(art)
	fmt.Fprintf(&out, `<g>`)
	for _, n := range s.nodes {
		if n["vis"] == false {
			continue
		}
		id, kind := str(n["id"]), str(n["t"])
		v := func(k string) int { return nodeInt(n, k) }
		x, y, w, h := v("x"), v("y"), v("w"), v("h")
		text, tint := html.EscapeString(str(n["s"])), str(n["c"])
		if tint == "" {
			tint = "#FFFAEE"
		}
		attr := ""
		if s.actions[id].kind != "" {
			attr = fmt.Sprintf(` data-id="%s" data-on="%s"`, id, str(n["on"]))
		}
		sw, stroke, fill := max(1, v("sw")), str(n["bc"]), tint
		if stroke == "" {
			stroke = "none"
		}
		if n["fill"] == false {
			fill, stroke = "none", tint
		}
		paint := fmt.Sprintf(` fill="%s" stroke="%s" stroke-width="%d"`, fill, stroke, sw)
		switch kind {
		case "rect", "btn":
			fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="%d" rx="%d"%s%s/>`, x, y, w, h, v("r"), paint, attr)
			if kind == "btn" {
				tc := str(n["tc"])
				if tc == "" {
					tc = catInk
				}
				fmt.Fprintf(&out, `<text x="%d" y="%d" fill="%s" text-anchor="middle" font-size="16" font-weight="bold">%s</text>`, x+w/2, y+h/2+6, tc, text)
			}
		case "circle":
			fmt.Fprintf(&out, `<circle cx="%d" cy="%d" r="%d"%s%s/>`, x, y, v("r"), paint, attr)
		case "line":
			fmt.Fprintf(&out, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="%d" stroke-linecap="round"%s/>`, v("x1"), v("y1"), v("x2"), v("y2"), tint, sw, attr)
		case "tri":
			fmt.Fprintf(&out, `<polygon points="%d,%d %d,%d %d,%d"%s%s/>`, v("x1"), v("y1"), v("x2"), v("y2"), v("x3"), v("y3"), paint, attr)
		case "arc":
			r, a, b := v("r"), v("a1"), v("a2")
			if b < a {
				b += 360
			}
			if b-a == 360 {
				fmt.Fprintf(&out, `<circle cx="%d" cy="%d" r="%d" fill="none" stroke="%s" stroke-width="%d"%s/>`, x, y, r, tint, sw, attr)
				break
			}
			point := func(a int) (float64, float64) {
				s, c := math.Sincos(float64(a) * math.Pi / 180)
				return float64(x) + float64(r)*c, float64(y) + float64(r)*s
			}
			ax, ay := point(a)
			bx, by := point(b)
			big := 0
			if b-a > 180 {
				big = 1
			}
			fmt.Fprintf(&out, `<path d="M%.3f %.3f A%d %d 0 %d 1 %.3f %.3f" fill="none" stroke="%s" stroke-width="%d" stroke-linecap="round"%s/>`, ax, ay, r, r, big, bx, by, tint, sw, attr)
		case "text":
			size := 14 * max(1, v("fs"))
			fmt.Fprintf(&out, `<text x="%d" y="%d" fill="%s" text-anchor="middle" font-size="%d" font-weight="bold"%s>%s</text>`, x, y+size/3, tint, size, attr, text)
		}
	}
	out.WriteString("</g>")
	out.WriteString("</svg>")
	return out.String()
}

type svgScreen struct {
	source string
	hits   []hitBox
	fb     []uint16
}

func buildScreen(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	root, hits, err := prepareSVG(raw, exit)
	if err != nil {
		return svgScreen{}, err
	}
	screen := svgScreen{source: svgString(root), hits: hits}
	if geometry.width != 0 {
		screen.fb, err = rasterSVG(screen.source, geometry, root.background())
	}
	return screen, err
}

func renderSVG(raw string) ([]uint16, []hitBox, error) {
	s, err := buildScreen(raw, false, roundScreen)
	return s.fb, s.hits, err
}

func (h *wsHub) svgFrame(d *wsDevice) wsFrame {
	return textFrame(object{"op": "svg", "rev": d.rev, "svg": d.svg.source})
}

func (h *wsHub) pushScreen(d *wsDevice, screen svgScreen, legacy object, full bool) {
	if !full && screen.source == d.svg.source && legacy == nil {
		return
	}
	d.rev++
	if d.client.role == "web" {
		d.svg = screen
		d.client.enqueue(h.svgFrame(d))
	} else if d.proto >= 2 {
		if screen.fb != nil {
			rect := dirtyRect(d.svg.fb, screen.fb, d.screen)
			if !rect.Empty() {
				d.client.enqueue(displayFrame(screen.fb, rect, d.rev, d.screen.width))
			}
		}
		d.svg = screen
	} else {
		d.svg = screen
		if legacy != nil {
			legacy["rev"] = d.rev
			d.client.enqueue(textFrame(legacy))
		}
	}
	for c := range h.clients {
		if c.watch == d.key {
			c.enqueue(h.svgFrame(d))
		}
	}
}

func (h *wsHub) renderSVGView(d *wsDevice, raw string, legacy object, full bool) {
	if !full && raw == d.svgRaw && legacy == nil {
		return
	}
	d.svgRaw = raw
	var geometry screenGeometry
	if d.proto >= 2 && d.client.role != "web" {
		geometry = d.screen
	}
	screen, err := buildScreen(raw, false, geometry)
	if err != nil {
		log.Printf("EchoEar screen: %v", err)
		if legacy == nil || d.proto >= 2 || d.client.role == "web" {
			return
		}
	}
	h.pushScreen(d, screen, legacy, full)
}
