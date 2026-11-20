package main

import (
	"fmt"
	"html"
	"log"
	"math"
	"strings"
)

const svgStart = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 360">`

func catSVG(view sceneView, cat catState) string { return sceneSVG(view, cat.SVGExtras) }
func drawerSVG(view sceneView) string            { return sceneSVG(view, "") }

func nestedSVG(raw string) string {
	if i := strings.IndexByte(raw, '>'); i >= 0 {
		return strings.TrimSuffix(raw[i+1:], "</svg>")
	}
	return ""
}

func sceneSVG(s sceneView, extras string) string {
	var out strings.Builder
	out.WriteString(svgStart)
	fmt.Fprintf(&out, `<rect width="360" height="360" fill="%s"/>`, s.bg)
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
	out.WriteString(nestedSVG(extras))
	out.WriteString("</svg>")
	return out.String()
}

type svgScreen struct {
	source string
	hits   []hitBox
	fb     *framebuffer
}

func buildScreen(raw string, exit, raster bool) (svgScreen, error) {
	svg, hits, err := prepareSVG(raw, exit)
	screen := svgScreen{source: svg, hits: hits}
	if err != nil {
		return screen, err
	}
	if raster {
		fb, err := rasterSVG(svg)
		screen.fb = &fb
		return screen, err
	}
	return screen, nil
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
			rect := dirtyRect(d.svg.fb, screen.fb)
			if !rect.Empty() {
				d.client.enqueue(displayFrame(screen.fb, rect, d.rev))
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
	screen, err := buildScreen(raw, false, d.proto >= 2 && d.client.role != "web")
	if err != nil {
		log.Printf("EchoEar screen: %v", err)
		if legacy == nil || d.proto >= 2 || d.client.role == "web" {
			return
		}
	}
	h.pushScreen(d, screen, legacy, full)
}
