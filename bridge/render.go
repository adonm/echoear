package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"image"
	imagecolor "image/color"
	"image/draw"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/f64"
	"golang.org/x/image/math/fixed"
)

const screenSize = 360
const maxSVG = 64 << 10
const screenCacheSize = 32

type screenGeometry struct {
	width, height int
	shape         string
}

var roundScreen = screenGeometry{screenSize, screenSize, "round"}

func (s screenGeometry) inverseTap(x, y int) (int, int) {
	side := min(s.width, s.height)
	// Offsets are (dimension - side) / 2. Keep half-pixel offsets exact.
	inverse := func(p, dimension int) int {
		return min(screenSize-1, max(0, (2*p-dimension+side)*screenSize/(2*side)))
	}
	return inverse(x, s.width), inverse(y, s.height)
}

func inScreenCircle(x, y int) bool {
	const r = screenSize / 2
	return (x-r)*(x-r)+(y-r)*(y-r) <= r*r
}

type hitBox struct {
	id             string
	x1, y1, x2, y2 int
	bounds         svgBounds // Transformed control bounds before touch padding and canvas clipping.
}

type svgScreen struct {
	raw, source string
	hits        []hitBox
	fb          []uint16
	render      time.Duration
}

// A prepared scene owns a normalized SVG tree and its topmost-first controls.
// Neither backend mutates it.
type preparedScene struct {
	root       *svgNode
	svg        string
	hits       []hitBox
	background imagecolor.Color
}

func (s preparedScene) toBrowserSVG() string { return s.svg }

type screenCacheKey struct {
	hash     [sha256.Size]byte
	geometry screenGeometry
	exit     bool
}

// Cached screens and their slices are immutable after publication.
type screenCache struct {
	mu           sync.Mutex
	entries      map[screenCacheKey]svgScreen
	keys         [screenCacheSize]screenCacheKey
	next         int
	hits, misses uint64
}

var screens screenCache

func buildScreen(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	return screens.build(raw, exit, geometry)
}

func (c *screenCache) build(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	start := time.Now()
	key := screenCacheKey{svgHash(raw), geometry, exit}
	c.mu.Lock()
	screen, ok := c.entries[key]
	if ok {
		c.hits++
		c.mu.Unlock()
		screen.render = time.Since(start)
		return screen, nil
	}
	c.misses++
	c.mu.Unlock()

	screen, err := buildScreenUncached(raw, exit, geometry)
	if err != nil {
		return svgScreen{}, err
	}
	c.mu.Lock()
	if cached, ok := c.entries[key]; ok {
		screen = cached
	} else {
		if c.entries == nil {
			c.entries = make(map[screenCacheKey]svgScreen, screenCacheSize)
		}
		if len(c.entries) == screenCacheSize {
			delete(c.entries, c.keys[c.next])
		}
		c.keys[c.next] = key
		c.next = (c.next + 1) % screenCacheSize
		c.entries[key] = screen
	}
	c.mu.Unlock()
	screen.render = time.Since(start)
	return screen, nil
}

func svgHash(raw string) [sha256.Size]byte {
	// Sum256 only reads the input; hashing the immutable string needs no copy.
	return sha256.Sum256(unsafe.Slice(unsafe.StringData(raw), len(raw)))
}

func buildScreenUncached(raw string, exit bool, geometry screenGeometry) (svgScreen, error) {
	return buildScreenWithPhotos(raw, exit, geometry, nil)
}

func buildScreenWithPhotos(raw string, exit bool, geometry screenGeometry, lookup photoLookup) (svgScreen, error) {
	scene, err := prepareScene(raw, exit)
	if err != nil {
		return svgScreen{}, err
	}
	screen := svgScreen{raw: raw, source: scene.toBrowserSVG(), hits: scene.hits}
	if geometry.width != 0 {
		screen.fb, err = scene.toRGB565(geometry, lookup)
	}
	return screen, err
}

func (c *screenCache) stats() (hits, misses uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

func renderSVG(raw string) ([]uint16, []hitBox, error) {
	s, err := buildScreen(raw, false, roundScreen)
	return s.fb, s.hits, err
}

func hitID(hits []hitBox, x, y int) string {
	for _, h := range hits {
		if x >= h.x1 && x <= h.x2 && y >= h.y1 && y <= h.y2 {
			return h.id
		}
	}
	return ""
}

func layoutWarnings(hits []hitBox, round bool) []string {
	var exits []hitBox
	for _, h := range hits {
		if h.id == exitNode {
			exits = append(exits, h)
		}
	}
	const r = screenSize / 2
	outside := func(x, y, radius float64) bool {
		return (x-r)*(x-r)+(y-r)*(y-r) > radius*radius
	}
	var warnings []string
	for _, h := range hits {
		if h.id == exitNode {
			continue
		}
		if slices.ContainsFunc(exits, func(e hitBox) bool {
			return h.x1 <= e.x2 && e.x1 <= h.x2 && h.y1 <= e.y2 && e.y1 <= h.y2
		}) {
			warnings = append(warnings, fmt.Sprintf("control %q overlaps bottom Cat exit button (keep controls above y=296)", h.id))
		}
		if round {
			b := h.bounds
			corners := 0
			for _, x := range [...]float64{b.x1, b.x2} {
				for _, y := range [...]float64{b.y1, b.y2} {
					if outside(x, y, r) {
						corners++
					}
				}
			}
			if outside((b.x1+b.x2)/2, (b.y1+b.y2)/2, r-6) || corners >= 2 {
				warnings = append(warnings, fmt.Sprintf("control %q clips outside the round display bezel", h.id))
			}
		}
	}
	return warnings
}

type svgNode struct {
	tag, text string
	attr      map[string]string
	children  []*svgNode
}

func (n *svgNode) asGroup() *svgNode {
	n.tag = "g"
	delete(n.attr, "viewBox")
	delete(n.attr, "xmlns")
	return n
}

var (
	svgTags           = wordSet("svg g defs linearGradient radialGradient stop path rect circle ellipse line polygon polyline text image")
	svgAttrs          = wordSet("d x y x1 y1 x2 y2 width height rx ry r cx cy fx fy points fill fill-opacity fill-rule stroke stroke-opacity stroke-width stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity transform gradientTransform gradientUnits spreadMethod offset stop-color stop-opacity font-size font-weight font-family text-anchor aria-label data-id data-hit data-pose")
	svgNumber         = regexp.MustCompile(`[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`)
	svgPath           = regexp.MustCompile(`^[MmZzLlHhVvCcSsQqTtAa0-9Ee.,+\s-]*$`)
	svgScalar         = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?(?:%|px)?$`)
	svgList           = regexp.MustCompile(`^[0-9Ee.,+\s-]+$`)
	svgPaint          = regexp.MustCompile(`^(?:[A-Za-z]+|#[0-9A-Fa-f]{3}|#[0-9A-Fa-f]{6}|rgb\([0-9.,% ]+\))$`)
	svgRef            = regexp.MustCompile(`^url\(#([A-Za-z0-9_.-]{1,64})\)$`)
	svgID             = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)
	svgTransform      = regexp.MustCompile(`^\s*(matrix|translate|scale|rotate|skewX|skewY)\s*\(([0-9Ee.,+\s-]+)\)\s*,?`)
	regularSVGFont, _ = sfnt.Parse(goregular.TTF)
	monoSVGFont, _    = sfnt.Parse(gomono.TTF)
	boldSVGFont, _    = sfnt.Parse(gobold.TTF)
)

func wordSet(words string) map[string]bool {
	m := map[string]bool{}
	for _, word := range strings.Fields(words) {
		m[word] = true
	}
	return m
}

func svgNums(s string) []float64 {
	var nums []float64
	for _, raw := range svgNumber.FindAllString(s, -1) {
		v, _ := strconv.ParseFloat(raw, 64)
		nums = append(nums, v)
	}
	return nums
}

func svgNum(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "px"), 64)
	return v
}

func svgMatrix(raw string) (rasterx.Matrix2D, bool) {
	m := rasterx.Identity
	for len(strings.TrimSpace(raw)) != 0 {
		part := svgTransform.FindStringSubmatch(raw)
		if part == nil {
			return m, false
		}
		n := svgNums(part[2])
		raw = raw[len(part[0]):]
		switch part[1] {
		case "matrix":
			if len(n) != 6 {
				return m, false
			}
			m = m.Mult(rasterx.Matrix2D{A: n[0], B: n[1], C: n[2], D: n[3], E: n[4], F: n[5]})
		case "translate":
			if len(n) < 1 || len(n) > 2 {
				return m, false
			}
			m = m.Translate(n[0], append(n, 0)[1])
		case "scale":
			if len(n) < 1 || len(n) > 2 {
				return m, false
			}
			m = m.Scale(n[0], append(n, n[0])[1])
		case "rotate":
			if len(n) != 1 && len(n) != 3 {
				return m, false
			}
			if len(n) == 3 {
				m = m.Translate(n[1], n[2])
			}
			m = m.Rotate(n[0] * math.Pi / 180)
			if len(n) == 3 {
				m = m.Translate(-n[1], -n[2])
			}
		case "skewX", "skewY":
			if len(n) != 1 {
				return m, false
			}
			t := math.Tan(n[0] * math.Pi / 180)
			if part[1] == "skewX" {
				m = m.Mult(rasterx.Matrix2D{A: 1, C: t, D: 1})
			} else {
				m = m.Mult(rasterx.Matrix2D{A: 1, B: t, D: 1})
			}
		}
	}
	return m, boundedSVGMatrix(m)
}

func boundedSVGMatrix(m rasterx.Matrix2D) bool {
	for _, n := range []float64{m.A, m.B, m.C, m.D} {
		if !(math.Abs(n) <= 12) {
			return false
		}
	}
	return math.Abs(m.E) <= 4096 && math.Abs(m.F) <= 4096
}

func cleanSVGAttr(k, v string) (string, bool) {
	switch k {
	case "id":
		return v, svgID.MatchString(v)
	case "data-id":
		return v, svgID.MatchString(v)
	case "aria-label":
		return shortText(v, 128, false), v != ""
	case "data-pose":
		return v, true
	case "fill", "stroke", "stop-color":
		return v, svgPaint.MatchString(v) || k != "stop-color" && svgRef.MatchString(v)
	case "fill-rule", "stroke-linecap", "stroke-linejoin", "gradientUnits", "spreadMethod", "font-weight", "text-anchor":
		return v, svgPaint.MatchString(v) || k == "font-weight" && svgScalar.MatchString(v)
	case "font-family":
		return v, v == "monospace"
	case "transform", "gradientTransform":
		m, ok := svgMatrix(v)
		if !ok {
			return "", false
		}
		// oksvg collapses a one-argument scale on Y; share our SVG matrix with the rasterizer.
		return fmt.Sprintf("matrix(%g %g %g %g %g %g)", m.A, m.B, m.C, m.D, m.E, m.F), true
	case "d":
		if !svgPath.MatchString(v) {
			return "", false
		}
	case "stroke-dasharray":
		if v == "none" {
			return v, true
		}
		if !svgList.MatchString(v) {
			return "", false
		}
		for _, n := range svgNums(v) {
			if n < 0 || n > 360 {
				return "", false
			}
		}
		if !slices.ContainsFunc(svgNums(v), func(n float64) bool { return n >= 1 }) {
			return "", false
		}
	case "points", "data-hit":
		if !svgList.MatchString(v) {
			return "", false
		}
	default:
		if !svgScalar.MatchString(v) {
			return "", false
		}
	}
	for _, n := range svgNums(v) {
		if math.Abs(n) > 4096 {
			return "", false
		}
	}
	return v, true
}

func parseSVG(raw string) (*svgNode, error) {
	if len(raw) > maxSVG {
		return nil, fmt.Errorf("SVG exceeds 64 KB")
	}
	dec := xml.NewDecoder(strings.NewReader(raw))
	var root *svgNode
	var stack []*svgNode
	nodes, photos := 0, 0
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("SVG: %w", err)
		}
		switch t := token.(type) {
		case xml.StartElement:
			if len(stack) == 0 && (root != nil || t.Name.Local != "svg") {
				return nil, fmt.Errorf("SVG requires one <svg viewBox=\"0 0 360 360\"> root")
			}
			tag := t.Name.Local
			if len(stack) > 24 || nodes >= 512 {
				return nil, fmt.Errorf("SVG too complex")
			}
			if !svgTags[tag] || t.Name.Space != "" && t.Name.Space != "http://www.w3.org/2000/svg" ||
				len(stack) != 0 && (tag == "svg" || stack[len(stack)-1].tag == "text" || stack[len(stack)-1].tag == "image") {
				if len(stack) == 0 {
					return nil, fmt.Errorf("Invalid SVG namespace")
				}
				if err := dec.Skip(); err != nil {
					return nil, fmt.Errorf("SVG: %w", err)
				}
				continue
			}
			n := &svgNode{tag: tag, attr: map[string]string{}}
			for _, a := range t.Attr {
				k, v := a.Name.Local, strings.TrimSpace(a.Value)
				if a.Name.Space != "" {
					continue
				}
				if tag == "image" && k == "href" {
					if id, ok := strings.CutPrefix(v, "photo:"); ok && photoID.MatchString(id) {
						n.attr[k] = v
					}
				}
				if len(stack) == 0 && k == "viewBox" && !slices.Equal(svgNums(v), []float64{0, 0, 360, 360}) {
					return nil, fmt.Errorf(`SVG viewBox must be "0 0 360 360"`)
				}
				if svgAttrs[k] || k == "id" && (tag == "linearGradient" || tag == "radialGradient") {
					if tag == "image" && strings.HasSuffix(v, "%") && (k == "x" || k == "y" || k == "width" || k == "height") {
						continue
					}
					if v, ok := cleanSVGAttr(k, v); ok {
						n.attr[k] = v
					}
				}
			}
			if tag == "image" {
				if n.attr["href"] == "" {
					if err := dec.Skip(); err != nil {
						return nil, fmt.Errorf("SVG: %w", err)
					}
					continue
				}
				photos++
				if photos > 8 {
					return nil, fmt.Errorf("SVG exceeds 8 photo references")
				}
			}
			if len(stack) == 0 {
				root = n
				root.attr["viewBox"] = "0 0 360 360"
				root.attr["xmlns"] = "http://www.w3.org/2000/svg"
				delete(root.attr, "width")
				delete(root.attr, "height")
			} else {
				parent := stack[len(stack)-1]
				if parent.tag == "defs" && tag != "linearGradient" && tag != "radialGradient" ||
					(parent.tag == "linearGradient" || parent.tag == "radialGradient") && tag != "stop" {
					if err := dec.Skip(); err != nil {
						return nil, fmt.Errorf("SVG: %w", err)
					}
					continue
				}
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
			nodes++
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1].tag == "text" {
				n := stack[len(stack)-1]
				n.text += string(t)
				if runeLen(n.text) > 256 {
					return nil, fmt.Errorf("SVG text exceeds 256 characters")
				}
			} else if len(stack) == 0 && strings.TrimSpace(string(t)) != "" {
				return nil, fmt.Errorf("Unexpected text outside SVG")
			}
		}
	}
	if root == nil {
		return nil, fmt.Errorf("Missing SVG")
	}
	return root, nil
}

func (n *svgNode) encode(enc *xml.Encoder) {
	start := xml.StartElement{Name: xml.Name{Local: n.tag}}
	keys := make([]string, 0, len(n.attr))
	for k := range n.attr {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: k}, Value: n.attr[k]})
	}
	_ = enc.EncodeToken(start)
	if n.text != "" {
		_ = enc.EncodeToken(xml.CharData(n.text))
	}
	for _, child := range n.children {
		child.encode(enc)
	}
	_ = enc.EncodeToken(start.End())
}

func svgString(n *svgNode) string {
	return encodeSVG(n, "")
}

func encodeSVG(n *svgNode, indent string) string {
	var out strings.Builder
	enc := xml.NewEncoder(&out)
	enc.Indent("", indent)
	n.encode(enc)
	_ = enc.Flush()
	return out.String()
}

type svgBounds struct{ x1, y1, x2, y2 float64 }

func (n *svgNode) bounds() (svgBounds, bool) {
	v := func(k string) float64 { return svgNum(n.attr[k]) }
	x, y := v("x"), v("y")
	if p := svgNums(n.attr["data-hit"]); len(p) == 4 && p[2] > 0 && p[3] > 0 {
		return svgBounds{p[0], p[1], p[0] + p[2], p[1] + p[3]}, true
	}
	switch n.tag {
	case "rect", "image":
		return svgBounds{x, y, x + v("width"), y + v("height")}, v("width") > 0 && v("height") > 0
	case "circle", "ellipse":
		x, y = v("cx"), v("cy")
		rx, ry := v("r"), v("r")
		if n.tag == "ellipse" {
			rx, ry = v("rx"), v("ry")
		}
		return svgBounds{x - rx, y - ry, x + rx, y + ry}, rx > 0 && ry > 0
	case "polygon", "polyline":
		p := svgNums(n.attr["points"])
		if len(p) < 4 || len(p)%2 != 0 {
			break
		}
		b := svgBounds{p[0], p[1], p[0], p[1]}
		for i := 2; i < len(p); i += 2 {
			b = svgBounds{min(b.x1, p[i]), min(b.y1, p[i+1]), max(b.x2, p[i]), max(b.y2, p[i+1])}
		}
		return b, true
	case "line":
		return svgBounds{min(v("x1"), v("x2")), min(v("y1"), v("y2")), max(v("x1"), v("x2")), max(v("y1"), v("y2"))}, true
	}
	return svgBounds{}, false
}

// Outlining text at its original z-order gives browser and board identical Go
// font metrics without requiring a font download or a separate text renderer.
type svgTextStyle [4]string

var svgTextAttrs = [...]string{"font-size", "font-weight", "font-family", "text-anchor"}

func fitLabel(text string, size, width int) int {
	var buf sfnt.Buffer
	ppem := fixed.I(size)
	var advance fixed.Int26_6
	for _, r := range text {
		glyph, _ := boldSVGFont.GlyphIndex(&buf, r)
		w, _ := boldSVGFont.GlyphAdvance(&buf, glyph, ppem, font.HintingNone)
		advance += w
	}
	return min(size, size*width/max(1, advance.Ceil()))
}

func outlineText(n *svgNode, style svgTextStyle) (svgBounds, bool) {
	f := regularSVGFont
	size, weight, family, anchor := svgNum(style[0]), style[1], style[2], style[3]
	if family == "monospace" {
		f = monoSVGFont
	} else if weight == "bold" || svgNum(weight) >= 600 {
		f = boldSVGFont
	}
	if size == 0 {
		size = 16
	}
	size = min(120, max(1, size))
	ppem := fixed.Int26_6(size * 64)
	x, y := svgNum(n.attr["x"]), svgNum(n.attr["y"])
	b := svgBounds{math.Inf(1), y - size, math.Inf(-1), y + size*.22}
	var buf sfnt.Buffer
	var d strings.Builder
	compact := func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }
	for _, line := range strings.Split(strings.TrimSpace(n.text), "\n") {
		chars := []rune(strings.Join(strings.Fields(line), " "))
		glyphs := make([]sfnt.GlyphIndex, len(chars))
		offsets := make([]fixed.Int26_6, len(chars))
		var width fixed.Int26_6
		for i, r := range chars {
			glyphs[i], _ = f.GlyphIndex(&buf, r)
			if i > 0 {
				k, _ := f.Kern(&buf, glyphs[i-1], glyphs[i], ppem, font.HintingNone)
				width += k
			}
			offsets[i] = width
			w, _ := f.GlyphAdvance(&buf, glyphs[i], ppem, font.HintingNone)
			width += w
		}
		start := x
		w := float64(width) / 64
		switch anchor {
		case "middle":
			start -= w / 2
		case "end":
			start -= w
		}
		b.x1, b.x2, b.y2 = min(b.x1, start), max(b.x2, start+w), y+size*.22
		for i, glyph := range glyphs {
			segments, _ := f.LoadGlyph(&buf, glyph, ppem, nil)
			for j, s := range segments {
				if j > 0 && s.Op == sfnt.SegmentOpMoveTo {
					d.WriteByte('Z')
				}
				d.WriteByte("MLQC"[s.Op])
				count := 1
				if s.Op >= sfnt.SegmentOpQuadTo {
					count = int(s.Op)
				}
				for _, p := range s.Args[:count] {
					d.WriteString(compact(start + float64(offsets[i]+p.X)/64))
					d.WriteByte(' ')
					d.WriteString(compact(y + float64(p.Y)/64))
					d.WriteByte(' ')
				}
			}
			if len(segments) != 0 {
				d.WriteByte('Z')
			}
		}
		y += size * 1.2
	}
	n.attr["aria-label"] = strings.Join(strings.Fields(n.text), " ")
	n.tag, n.text, n.attr["d"] = "path", "", d.String()
	return b, b.x2 > b.x1
}

func prepareScene(raw string, exit bool) (preparedScene, error) {
	root, err := parseSVG(raw)
	if err != nil {
		return preparedScene{}, err
	}
	var hits []hitBox
	hasExit := false
	var walk func(*svgNode, rasterx.Matrix2D, svgTextStyle, bool, int) error
	walk = func(n *svgNode, m rasterx.Matrix2D, style svgTextStyle, defs bool, pad int) error {
		defs = defs || n.tag == "defs" || n.tag == "linearGradient" || n.tag == "radialGradient" || n.tag == "stop"
		if raw := n.attr["transform"]; raw != "" {
			local, _ := svgMatrix(raw)
			m = m.Mult(local)
			if !boundedSVGMatrix(m) {
				return fmt.Errorf("SVG combined transform is too large")
			}
		}
		if n.attr["id"] != "" && !strings.HasPrefix(n.attr["id"], "echoear-paint-") {
			n.attr["id"] = "echoear-paint-" + n.attr["id"]
		}
		for _, k := range []string{"fill", "stroke"} {
			if ref := n.attr[k]; strings.HasPrefix(ref, "url(#") && !strings.HasPrefix(ref, "url(#echoear-paint-") {
				n.attr[k] = "url(#echoear-paint-" + ref[5:]
			}
		}
		for i, k := range svgTextAttrs {
			if v := n.attr[k]; v != "" {
				style[i] = v
				delete(n.attr, k)
			}
		}
		id := n.attr["data-id"]
		var b svgBounds
		var ok bool
		if id != "" && !defs {
			b, ok = n.bounds()
		}
		if n.tag == "text" {
			metrics, measured := outlineText(n, style)
			if !ok {
				b, ok = metrics, measured
			}
			if len(n.attr["d"]) > 64<<10 {
				return fmt.Errorf("SVG text outlines too large")
			}
		}
		if !defs && id != "" && ok {
			out := svgBounds{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
			for _, p := range [][2]float64{{b.x1, b.y1}, {b.x2, b.y1}, {b.x1, b.y2}, {b.x2, b.y2}} {
				x, y := m.Transform(p[0], p[1])
				out = svgBounds{min(x, out.x1), min(y, out.y1), max(x, out.x2), max(y, out.y2)}
			}
			const last = screenSize - 1
			h := hitBox{id, max(0, int(math.Floor(out.x1))-pad), max(0, int(math.Floor(out.y1))-pad),
				min(last, int(math.Ceil(out.x2))+pad), min(last, int(math.Ceil(out.y2))+pad), out}
			if h.x1 <= h.x2 && h.y1 <= h.y2 {
				hits = append(hits, h)
				hasExit = hasExit || id == exitNode
				// The browser uses these logical bounds, not DOM geometry, for pointer and keyboard input.
				n.attr["data-echoear-hit"] = fmt.Sprintf("%d %d %d %d", h.x1, h.y1, h.x2, h.y2)
			}
		}
		if n.attr["data-echoear-hit"] == "" {
			delete(n.attr, "data-id")
		}
		for _, child := range n.children {
			if err := walk(child, m, style, defs, pad); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, rasterx.Identity, svgTextStyle{}, false, 8); err != nil {
		return preparedScene{}, err
	}
	background := root.background()
	if exit && !hasExit {
		pill, _ := parseSVG(`<svg><g data-id="echoear-exit" data-hit="137,304,86,32"><rect x="137" y="304" width="86" height="32" rx="16" fill="#FFFAEE" stroke="#1A1926" stroke-width="3"/><text x="180" y="326" text-anchor="middle" font-size="19" font-weight="bold" fill="#1A1926">Cat</text></g></svg>`)
		_ = walk(pill, rasterx.Identity, svgTextStyle{}, false, 4)
		if root.attr["transform"] != "" || root.attr["opacity"] != "" {
			// Keep the bridge's escape hatch outside the author's transform/opacity.
			pill.children = append([]*svgNode{root.asGroup()}, pill.children...)
			root = pill
		} else {
			root.children = append(root.children, pill.children...)
		}
	}
	slices.Reverse(hits)
	return preparedScene{root, svgString(root), hits, background}, nil
}

// A leading, solid full-viewBox rectangle is the SVG canvas background.
func (root *svgNode) background() imagecolor.Color {
	for _, n := range root.children {
		if n.tag == "defs" {
			continue
		}
		b, _ := n.bounds()
		if n.tag == "rect" && b == (svgBounds{0, 0, screenSize, screenSize}) &&
			n.attr["transform"] == "" && root.attr["transform"] == "" {
			fill := n.attr["fill"]
			if fill == "" {
				fill = root.attr["fill"]
			}
			if c, err := oksvg.ParseSVGColor(fill); c != nil && err == nil {
				opacity := 1.0
				for _, attrs := range []map[string]string{root.attr, n.attr} {
					for _, key := range []string{"opacity", "fill-opacity"} {
						if v := attrs[key]; v != "" {
							opacity *= min(1, max(0, svgNum(v)))
						}
					}
				}
				r, g, b, _ := c.RGBA()
				return imagecolor.RGBA{uint8(float64(r>>8) * opacity), uint8(float64(g>>8) * opacity), uint8(float64(b>>8) * opacity), 255}
			}
		}
		break
	}
	return imagecolor.Black
}

type screenScanner struct {
	*rasterx.ScannerGV
	dst *image.RGBA
}

func (s screenScanner) Draw() {
	// ScannerGV composites the whole destination each time. Shrink the
	// destination without moving its origin: the coverage mask and gradient
	// coordinates still belong to the original canvas.
	extent := s.GetPathExtent()
	dst := *s.dst
	dst.Rect.Max = image.Pt(min(dst.Rect.Max.X, extent.Max.X.Ceil()+1), min(dst.Rect.Max.Y, extent.Max.Y.Ceil()+1))
	if !dst.Rect.Empty() {
		s.Dest = &dst
		s.ScannerGV.Draw()
	}
}

func (s preparedScene) toRGB565(screen screenGeometry, lookup photoLookup) (fb []uint16, err error) {
	// oksvg and rasterx are also fed untrusted geometry.
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("Invalid SVG geometry")
		}
	}()
	w, h := screen.width, screen.height
	side := min(w, h)
	x, y := float64(w-side)/2, float64(h-side)/2
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
	scan := screenScanner{rasterx.NewScannerGV(w, h, img, img.Bounds()), img}
	clip := image.Rect(int(x), int(y), int(math.Ceil(x+float64(side))), int(math.Ceil(y+float64(side))))
	drawSVG := func(svg string) error {
		icon, err := oksvg.ReadIconStream(strings.NewReader(svg), oksvg.StrictErrorMode)
		if err != nil {
			return fmt.Errorf("SVG: %w", err)
		}
		icon.SetTarget(x, y, float64(side), float64(side))
		icon.Draw(rasterx.NewDasher(w, h, scan), 1)
		return nil
	}
	if !strings.Contains(s.svg, "<image ") {
		err = drawSVG(s.svg)
	} else {
		scale := float64(side) / screenSize
		err = rasterPhotos(s.root, img, rasterx.Identity.Translate(x, y).Scale(scale, scale), lookup, drawSVG)
	}
	if err != nil {
		return nil, err
	}
	fb = make([]uint16, w*h)
	br, bgc, bb, _ := s.background.RGBA()
	bar := uint16(br>>11)<<11 | uint16(bgc>>10)<<5 | uint16(bb>>11)
	for y := range h {
		for x := range w {
			dx, dy := 2*x-w, 2*y-h
			if screen.shape == "rect" && !image.Pt(x, y).In(clip) {
				fb[y*w+x] = bar
			} else if screen.shape == "rect" || dx*dx+dy*dy <= side*side {
				i := y*img.Stride + x*4
				fb[y*w+x] = uint16(img.Pix[i]&0xf8)<<8 | uint16(img.Pix[i+1]&0xfc)<<3 | uint16(img.Pix[i+2])>>3
			}
		}
	}
	return fb, nil
}

type photoLookup func(string) (image.Image, error)

// oksvg draws vector paths only. Flush consecutive vector layers on either
// side of a bitmap, preserving groups, transforms and painter order.
func rasterPhotos(root *svgNode, dst *image.RGBA, target rasterx.Matrix2D, lookup photoLookup, drawSVG func(string) error) error {
	var defs []*svgNode
	var collectDefs func(*svgNode)
	collectDefs = func(n *svgNode) {
		for _, child := range n.children {
			if child.tag == "defs" {
				defs = append(defs, child)
			} else {
				collectDefs(child)
			}
		}
	}
	collectDefs(root)
	layer := &svgNode{tag: root.tag, attr: root.attr, children: slices.Clone(defs)}
	groups := map[*svgNode]*svgNode{root: layer}
	flush := func() error {
		if len(layer.children) == len(defs) {
			return nil
		}
		err := drawSVG(svgString(layer))
		layer.children = layer.children[:len(defs)]
		clear(groups)
		groups[root] = layer
		return err
	}
	images := make(map[string]image.Image)
	var walk func(*svgNode, []*svgNode, rasterx.Matrix2D, float64) error
	walk = func(n *svgNode, parents []*svgNode, m rasterx.Matrix2D, opacity float64) error {
		local, _ := svgMatrix(n.attr["transform"])
		m = m.Mult(local)
		if s := n.attr["opacity"]; s != "" {
			opacity *= min(1, max(0, svgNum(s)))
		}
		switch n.tag {
		case "defs":
		case "svg", "g":
			for _, child := range n.children {
				if err := walk(child, append(parents, n), m, opacity); err != nil {
					return err
				}
			}
		case "image":
			if lookup == nil || opacity <= 0 {
				return nil
			}
			width, height := svgNum(n.attr["width"]), svgNum(n.attr["height"])
			if width <= 0 || height <= 0 {
				return nil
			}
			id := strings.TrimPrefix(n.attr["href"], "photo:")
			src, ok := images[id]
			if !ok {
				var err error
				src, err = lookup(id)
				if err != nil {
					return err
				}
				images[id] = src
			}
			if src == nil {
				return nil
			}
			if err := flush(); err != nil {
				return err
			}
			b := src.Bounds()
			scale := min(width/float64(b.Dx()), height/float64(b.Dy()))
			x := svgNum(n.attr["x"]) + (width-scale*float64(b.Dx()))/2
			y := svgNum(n.attr["y"]) + (height-scale*float64(b.Dy()))/2
			m = m.Translate(x, y).Scale(scale, scale).Translate(-float64(b.Min.X), -float64(b.Min.Y))
			if m.A*m.D-m.B*m.C != 0 {
				var opts *xdraw.Options
				if opacity < 1 {
					opts = &xdraw.Options{SrcMask: image.NewUniform(imagecolor.Alpha{A: uint8(255 * opacity)})}
				}
				xdraw.BiLinear.Transform(dst, f64.Aff3{m.A, m.C, m.E, m.B, m.D, m.F}, src, b, draw.Over, opts)
			}
		default:
			parent := layer
			for _, ancestor := range parents {
				group := groups[ancestor]
				if group == nil {
					group = &svgNode{tag: ancestor.tag, attr: ancestor.attr}
					parent.children = append(parent.children, group)
					groups[ancestor] = group
				}
				parent = group
			}
			parent.children = append(parent.children, n)
		}
		return nil
	}
	if err := walk(root, nil, target, 1); err != nil {
		return err
	}
	return flush()
}

func dirtyRect(prev, next []uint16, screen screenGeometry) image.Rectangle {
	w, h := screen.width, screen.height
	if prev == nil {
		return image.Rect(0, 0, w, h)
	}
	minX, minY, maxX, maxY := w, h, 0, 0
	for i, pixel := range next {
		if pixel != prev[i] {
			x, y := i%w, i/w
			minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x+1), max(maxY, y+1)
		}
	}
	if minX >= maxX {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX, maxY)
}

func encodeRLE565(p []uint16) []byte {
	out := make([]byte, 0, len(p)*2)
	for i := 0; i < len(p); {
		start := i
		i++
		if i < len(p) && p[i] == p[start] {
			for i < len(p) && i-start < 128 && p[i] == p[start] {
				i++
			}
			out = append(out, 0x80|byte(i-start-1), byte(p[start]>>8), byte(p[start]))
		} else {
			for i < len(p) && i-start < 128 && (i+1 == len(p) || p[i] != p[i+1]) {
				i++
			}
			out = append(out, byte(i-start-1))
			for _, v := range p[start:i] {
				out = binary.BigEndian.AppendUint16(out, v)
			}
		}
	}
	return out
}

func displayFrame(fb []uint16, r image.Rectangle, rev uint64, width int) wsFrame {
	p := make([]uint16, 0, r.Dx()*r.Dy())
	for y := r.Min.Y; y < r.Max.Y; y++ {
		p = append(p, fb[y*width+r.Min.X:y*width+r.Max.X]...)
	}
	out := make([]byte, 12, 12+len(p)*2)
	out[0] = 0x03
	for i, v := range []int{r.Min.X, r.Min.Y, r.Dx(), r.Dy()} {
		binary.LittleEndian.PutUint16(out[1+i*2:], uint16(v))
	}
	binary.LittleEndian.PutUint16(out[10:], uint16(rev))
	if encoded := encodeRLE565(p); len(encoded) <= len(p)*2 {
		out[9] = 1
		out = append(out, encoded...)
	} else {
		for _, v := range p {
			out = binary.BigEndian.AppendUint16(out, v)
		}
	}
	return wsFrame{wsBinary, out}
}
