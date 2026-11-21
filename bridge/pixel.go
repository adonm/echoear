package main

import (
	"fmt"
	"image"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var larkPalette = map[string]string{
	".": "transparent", "k": "#1A1722", "w": "#FAF6F0", "p": "#F5B895",
	"m": "#5EEAD4", "l": "#C4B5FD", "s": "#7DD3FC", "y": "#FDE047",
	"r": "#FB7185", "g": "#4ADE80", "a": "#F59E0B", "b": "#9A5B25",
	"n": "#64748B", "d": "#2B2438", "c": "#FBCFE8", "o": "#FB923C",
}

const maxSpriteSide = 64
const maxSpriteRuns = maxSpriteSide * maxSpriteSide

var pixelKey = regexp.MustCompile(`^[!-~]$`)
var pixelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type companionSprite struct {
	Rows     []string          `json:"rows"`
	Blink    map[string]string `json:"blink,omitempty"`
	Talk     map[string]string `json:"talk,omitempty"`
	Think    map[string]string `json:"think,omitempty"`
	Sleep    map[string]string `json:"sleep,omitempty"`
	Palette  map[string]string `json:"palette,omitempty"`
	Mirror   bool              `json:"mirror,omitempty"`
	partTags bool              // Only the built-in cat knows which pixels are editable parts.
	listen   map[string]string
	bg       string
}

func spriteColors(overrides map[string]string) (map[string]string, error) {
	if len(overrides) == 0 {
		return larkPalette, nil
	}
	p := maps.Clone(larkPalette)
	for key, value := range overrides {
		if key == "." || !pixelKey.MatchString(key) || !pixelColor.MatchString(value) {
			return nil, fmt.Errorf("sprite palette: expected one printable ASCII key (not '.') and a #RRGGBB color, got %q", key)
		}
		p[key] = strings.ToUpper(value)
	}
	return p, nil
}

func spriteGrid(rows []string, p map[string]string) (int, error) {
	if len(rows) < 2 || len(rows) > maxSpriteSide || len(rows[0]) < 2 || len(rows[0]) > maxSpriteSide {
		return 0, fmt.Errorf("sprite grid must be 2–64 rows and columns")
	}
	w := len(rows[0])
	for i, row := range rows {
		if len(row) != w {
			return 0, fmt.Errorf("sprite row %d must have %d columns", i, w)
		}
		for j := range len(row) {
			if p[row[j:j+1]] == "" {
				return 0, fmt.Errorf("sprite row %d: unknown palette key %q", i, row[j:j+1])
			}
		}
	}
	return w, nil
}

func mirrorSpriteRows(rows []string) []string {
	full := make([]string, len(rows))
	for i, row := range rows {
		flip := []byte(row)
		slices.Reverse(flip)
		full[i] = row + string(flip)
	}
	return full
}

func compileSprite(rows []string, palette map[string]string, x, y, scale int, mirror ...bool) (string, error) {
	if len(mirror) > 0 && mirror[0] {
		rows = mirrorSpriteRows(rows)
	}
	p, err := spriteColors(palette)
	if err != nil {
		return "", err
	}
	if _, err := spriteGrid(rows, p); err != nil {
		return "", err
	}
	return spriteArtwork(rows, p, x, y, scale)
}

func spriteArtwork(rows []string, p map[string]string, x, y, scale int) (string, error) {
	if scale < 1 || scale > 128 || x < -1024 || y < -1024 || x > 1024 || y > 1024 {
		return "", fmt.Errorf("sprite position or scale out of range")
	}
	if len(rows) == 0 {
		return "", nil
	}
	dense := max(len(rows), len(rows[0])) > 16
	if dense && scale > 12 {
		return "", fmt.Errorf("sprites larger than 16 pixels need a scale of at most 12")
	}
	type colorPath struct {
		d    []byte
		x, y int
	}
	paths := map[string]colorPath{}
	var colors []string
	var out strings.Builder
	for j, row := range rows {
		for i := 0; i < len(row); {
			fill := p[row[i:i+1]]
			end := i + 1
			for end < len(row) && p[row[end:end+1]] == fill {
				end++
			}
			if fill != "transparent" {
				if dense {
					path := paths[fill]
					op, dx, dy := byte('m'), i-path.x, j-path.y
					if len(path.d) == 0 {
						colors = append(colors, fill)
						op, dx, dy = 'M', i, j
					}
					width := int64(end - i)
					path.d = strconv.AppendInt(append(path.d, op), int64(dx), 10)
					path.d = strconv.AppendInt(append(path.d, ' '), int64(dy), 10)
					path.d = strconv.AppendInt(append(path.d, 'h'), width, 10)
					path.d = strconv.AppendInt(append(path.d, 'v'), 1, 10)
					path.d = strconv.AppendInt(append(path.d, 'h'), -width, 10)
					path.d = append(path.d, 'Z')
					path.x, path.y = i, j
					paths[fill] = path
				} else {
					fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x+i*scale, y+j*scale, (end-i)*scale, scale, fill)
				}
			}
			i = end
		}
	}
	if len(colors) > 0 {
		fmt.Fprintf(&out, `<g shape-rendering="crispEdges" transform="translate(%d %d) scale(%d)">`, x, y, scale)
		for _, fill := range colors {
			fmt.Fprintf(&out, `<path data-pixel-runs="1" fill="%s" d="%s"/>`, fill, paths[fill].d)
		}
		out.WriteString("</g>")
		return out.String(), nil
	}
	if out.Len() == 0 {
		return "", nil
	}
	return `<g shape-rendering="crispEdges">` + out.String() + `</g>`, nil
}

// The path is also ordinary browser SVG. Accept only our exact, disjoint-run
// grammar on the native fast path; everything else remains a vector path.
func parsePixelRuns(d string) ([]image.Rectangle, bool) {
	at := 0
	take := func(ch byte) bool {
		if at == len(d) || d[at] != ch {
			return false
		}
		at++
		return true
	}
	number := func() (int, bool) {
		neg := take('-')
		start, n := at, 0
		for at < len(d) && d[at] >= '0' && d[at] <= '9' {
			n = 10*n + int(d[at]-'0')
			at++
			if at-start > 2 || n > maxSpriteSide {
				return 0, false
			}
		}
		if neg {
			n = -n
		}
		return n, at > start
	}
	var runs []image.Rectangle
	x, y, end := 0, 0, 0
	for at < len(d) && len(runs) < maxSpriteRuns {
		op := byte('m')
		if len(runs) == 0 {
			op = 'M'
		}
		if !take(op) {
			break
		}
		dx, ok1 := number()
		if !take(' ') {
			break
		}
		dy, ok2 := number()
		if !ok1 || !ok2 || !take('h') {
			break
		}
		width, ok := number()
		x, y = x+dx, y+dy
		// Compiler emits left-to-right non-overlapping runs in each row.
		if !ok || width < 1 || x < 0 || x+width > maxSpriteSide || y < 0 || y >= maxSpriteSide ||
			dy < 0 || len(runs) > 0 && dy == 0 && x < end || !take('v') || !take('1') || !take('h') {
			break
		}
		back, ok := number()
		if !ok || back != -width || !take('Z') {
			break
		}
		runs = append(runs, image.Rect(x, y, x+width, y+1))
		end = x + width
		if at == len(d) {
			return runs, true
		}
	}
	return nil, false
}

// The data-pose names are the existing face_svg protocol, not new mood names.
var pixelPoses = [...]string{"idle", "blink", "listen", "think", "talk", "sleep"}

func compileCompanionSprite(spec companionSprite) (string, error) {
	p, err := spriteColors(spec.Palette)
	if err != nil {
		return "", err
	}
	if spec.Mirror {
		spec.Rows = mirrorSpriteRows(spec.Rows)
	}
	w, err := spriteGrid(spec.Rows, p)
	if err != nil {
		return "", err
	}
	h := len(spec.Rows)
	var poses [len(pixelPoses)][]string
	for i, edits := range []map[string]string{nil, spec.Blink, spec.listen, spec.Think, spec.Talk, spec.Sleep} {
		rows := slices.Clone(spec.Rows)
		if i != 0 {
			if len(edits) == 0 && !spec.partTags {
				rows = autoSpritePose(rows, pixelPoses[i], p, spec.Mirror)
			}
			for key, row := range edits {
				j, err := strconv.Atoi(key)
				if err != nil || j < 0 || j >= h || strconv.Itoa(j) != key {
					return "", fmt.Errorf("sprite %s: invalid row index %q", pixelPoses[i], key)
				}
				if spec.Mirror {
					flip := []byte(row)
					slices.Reverse(flip)
					row += string(flip)
				}
				rows[j] = row
			}
		}
		if _, err := spriteGrid(rows, p); err != nil {
			return "", fmt.Errorf("%s: %w", pixelPoses[i], err)
		}
		poses[i] = rows
	}

	// Store stable pixels once; each face layer then paints only its variation.
	// A starter feature is classified and shared as a whole so face_edits can
	// replace it, including highlights that themselves don't animate.
	const maskCount = 7
	names := []string{"", "eyes", "mouth", "nose", "whiskers", "cheeks", "forehead"}
	if !spec.partTags {
		names = names[:1]
	}
	var masks [len(pixelPoses)][maskCount][]string
	for pose, rows := range poses {
		for i := range names {
			masks[pose][i] = make([]string, h)
		}
		for y, row := range rows {
			var line [maskCount][maxSpriteSide]byte
			for x := range w {
				for i := range names {
					line[i][x] = '.'
				}
				pixel := row[x]
				part := 0
				if spec.partTags {
					part = starterPart(x, y, pixel)
				}
				line[part][x] = pixel
				if part != 0 {
					line[0][x] = 'w'
					if part == 4 || part == 6 {
						line[0][x] = 'p'
					}
				}
			}
			for i := range names {
				masks[pose][i][y] = string(line[i][:w])
			}
		}
	}
	var fixed strings.Builder
	var body [len(pixelPoses)]strings.Builder
	extent := 192
	if max(w, h) > 16 {
		extent = 288
	}
	scale := 3 * (extent / max(w, h) / 3)
	if max(w, h) > 16 {
		scale = min(scale, 12)
	}
	x, y := (360-w*scale)/2, (360-h*scale)/2
	for i, name := range names {
		common := make([]string, h)
		if i == 0 {
			for j, row := range masks[0][i] {
				shared := []byte(row)
				for k := range w {
					for pose := 1; pose < len(poses); pose++ {
						if masks[pose][i][j][k] != row[k] {
							shared[k] = '.'
							break
						}
					}
				}
				common[j] = string(shared)
				for pose := range poses {
					layer := []byte(masks[pose][i][j])
					for k, c := range shared {
						if c != '.' {
							layer[k] = '.'
						}
					}
					masks[pose][i][j] = string(layer)
				}
			}
		} else {
			common = masks[0][i]
			for pose := 1; pose < len(poses); pose++ {
				if !slices.Equal(common, masks[pose][i]) {
					common = nil
					break
				}
			}
		}
		write := func(out *strings.Builder, rows []string) error {
			art, err := spriteArtwork(rows, p, x, y, scale)
			if art != "" {
				if name != "" {
					art = `<g data-part="` + name + `">` + art + `</g>`
				}
				out.WriteString(art)
			}
			return err
		}
		if err := write(&fixed, common); err != nil {
			return "", err
		}
		if i == 0 || common == nil {
			for pose := range poses {
				if err := write(&body[pose], masks[pose][i]); err != nil {
					return "", err
				}
			}
		}
	}
	var out strings.Builder
	out.WriteString(svgStart)
	if spec.bg != "" {
		out.WriteString(`<rect width="360" height="360" fill="` + spec.bg + `"/>`)
	}
	out.WriteString(fixed.String())
	for i, pose := range pixelPoses {
		fmt.Fprintf(&out, `<g data-pose="%s">%s</g>`, pose, body[i].String())
	}
	out.WriteString("</svg>")
	raw := out.String()
	if _, err := parseFaceSVG(raw); err != nil {
		return "", err
	}
	return raw, nil
}

func autoSpritePose(rows []string, pose string, p map[string]string, mirror bool) []string {
	// Prefer the existing eye and mouth region. Keep edits off the silhouette
	// where possible; a sprite with no face still gets a small color change.
	h, w := len(rows), len(rows[0])
	var changed [maxSpriteSide][maxSpriteSide]bool
	edit := func(x, y int, key byte) {
		if mirror {
			x = min(x, w-1-x)
		}
		best, col, row := 1<<30, -1, -1
		for j, line := range rows {
			for i := range len(line) {
				if mirror && i >= w/2 {
					break
				}
				if changed[j][i] || line[i] == '.' || p[line[i:i+1]] == p[string(key)] {
					continue
				}
				dx, dy := i-x, j-y
				score := 2 * (dx*dx + dy*dy)
				if line[i] != 'k' && line[i] != 'd' {
					score += 3
				}
				if score < best {
					best, col, row = score, i, j
				}
			}
		}
		if col >= 0 {
			changed[row][col] = true
			line := []byte(rows[row])
			line[col] = key
			if mirror {
				line[w-1-col] = key
			}
			rows[row] = string(line)
		}
	}
	switch pose {
	case "blink":
		edit(w/3, h/2, 'p')
		if !mirror {
			edit(w-1-w/3, h/2, 'p')
		}
	case "listen":
		edit(w/3, h/2, 'w')
		if !mirror {
			edit(w-1-w/3, h/2, 'w')
		}
	case "think":
		edit(w-1-w/3, h/2, 'l')
	case "talk":
		edit(w/2-1, h*3/4, 'k')
		edit(w/2, h*3/4, 'r')
	case "sleep":
		edit(w/3, h/2, 'p')
		if !mirror {
			edit(w-1-w/3, h/2, 'p')
		}
		edit(w/2, h*3/4, 'l')
	}
	return rows
}

var larkLogoSprite = []string{
	"................",
	"......kk.....a..",
	".....kpk......a.",
	"....kppkkk.....a",
	"...kppwwkwk..a.a",
	"...kppwwwwkk..a.",
	"...kppwwwwkak.a.",
	"..kppwwwckkk....",
	"..kpbpwwwwk.....",
	".kppbbpwwk......",
	"kpppbppwk.......",
	"kbbpppkk........",
	".kkkkkk.........",
	".....k.k........",
	"...bbbbbbbb.....",
	"................",
}

var defaultStarterCatSprite = companionSprite{
	Rows: []string{
		"................",
		"................",
		"....kk..........",
		"...kppk.........",
		"...kowpk........",
		"...kocwpk.......",
		"...koccpkkkkkkkk",
		"...koccppoppoppp",
		"...kpccppopooopp",
		"..kkpppppppooppp",
		"..kpppppwwwwwwww",
		"..kppppwwkwwwwww",
		"..kpppwwkkkkwwww",
		"..kppwwkkHkkkwww",
		"..kppwwkHmskkwww",
		"..kppwwkqqmkkwww",
		"..kppwwkmmqkkwww",
		"..kppwwfkkkkwwww",
		"..kpcccwwwwwwwrr",
		"kkbppccwwwwwwwrr",
		".kkbppcwwwwwwwwk",
		"..kkbppfwwwkkwwk",
		"...kkppfwwwwkkkw",
		"....kkppffwwwwww",
		"......kkkkkkkkkk",
		"................",
		"................",
		"................",
		"................",
		"................",
		"................",
		"................",
	},
	Mirror:  true,
	Palette: map[string]string{"H": "#FFFFFF", "q": "#28756D", "f": "#E8CFBD"},
	Blink: map[string]string{
		"12": "..kpppwwwwwwwwww", "13": "..kppwwwwwwwwwww", "14": "..kppwwkwwwwkwww",
		"15": "..kppwwwkkkkwwww", "16": "..kppwwffffffffw", "17": "..kppwwwwwwwwwww",
	},
	Talk: map[string]string{
		"20": ".kkbppcwwwwwwkkk", "21": "..kkbppfwwwwkkkk",
		"22": "...kkppfwwwwkrrr", "23": "....kkppffwwwkkk",
	},
	Think: map[string]string{
		"11": "..kppppwwkkwwwww", "14": "..kppwwkkHskkwww", "15": "..kppwwkkqskkwww",
		"20": ".kkbppcwwwwwwwww", "21": "..kkbppfwwwwwwkk", "22": "...kkppfwwwwwwww",
	},
	Sleep: map[string]string{
		"12": "..kpppwwwwwwwwww", "13": "..kppwwwwwwwwwww", "14": "..kppwwwwwwwwwww",
		"15": "..kppwwkwwwwkwww", "16": "..kppwwwkkkkwwww", "17": "..kppwwwffffwwww",
		"20": ".kkbppcwwwwwwwww", "21": "..kkbppfwwwwwwkk", "22": "...kkppfwwwwwkrr", "23": "....kkppffwwwwkk",
	},
	listen: map[string]string{
		"11": "..kppppwkkkwwwww", "12": "..kpppwwkHkkwwww",
		"20": ".kkbppcwwwwwwwww", "21": "..kkbppfwwwwwwkk", "22": "...kkppfwwwwwwkk",
	},
	partTags: true,
}

func starterPart(x, y int, c byte) int {
	x = min(x, 31-x)
	switch {
	case y >= 11 && y <= 17 && x >= 7 && x <= 12 && strings.ContainsRune("kHmqs", rune(c)):
		return 1
	case y >= 20 && y <= 23 && x >= 11 && (c == 'k' || c == 'r'):
		return 2
	case y >= 18 && y <= 19 && c == 'r':
		return 3
	case c == 'b':
		return 4
	case y >= 18 && c == 'c':
		return 5
	case y >= 7 && y <= 9 && x >= 9 && c == 'o':
		return 6
	}
	return 0
}
