package main

import (
	"fmt"
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

const maxSpriteRuns = 256

var pixelKey = regexp.MustCompile(`^[!-~]$`)
var pixelColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

type companionSprite struct {
	Rows     []string          `json:"rows"`
	Blink    map[string]string `json:"blink,omitempty"`
	Talk     map[string]string `json:"talk,omitempty"`
	Think    map[string]string `json:"think,omitempty"`
	Sleep    map[string]string `json:"sleep,omitempty"`
	Palette  map[string]string `json:"palette,omitempty"`
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
	if len(rows) < 2 || len(rows) > 24 || len(rows[0]) < 2 || len(rows[0]) > 24 {
		return 0, fmt.Errorf("sprite grid must be 2–24 rows and columns")
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

func compileSprite(rows []string, palette map[string]string, x, y, scale int) (string, error) {
	p, err := spriteColors(palette)
	if err != nil {
		return "", err
	}
	if _, err := spriteGrid(rows, p); err != nil {
		return "", err
	}
	return spriteRects(rows, p, x, y, scale)
}

func spriteRects(rows []string, p map[string]string, x, y, scale int) (string, error) {
	if scale < 1 || scale > 128 || x < -1024 || y < -1024 || x > 1024 || y > 1024 {
		return "", fmt.Errorf("sprite position or scale out of range")
	}
	var out strings.Builder
	count := 0
	for j, row := range rows {
		for i := 0; i < len(row); {
			fill := p[row[i:i+1]]
			end := i + 1
			for end < len(row) && p[row[end:end+1]] == fill {
				end++
			}
			if fill != "transparent" {
				count++
				if count > maxSpriteRuns {
					return "", fmt.Errorf("sprite exceeds %d rectangle runs", maxSpriteRuns)
				}
				fmt.Fprintf(&out, `<rect x="%d" y="%d" width="%d" height="%d" fill="%s"/>`, x+i*scale, y+j*scale, (end-i)*scale, scale, fill)
			}
			i = end
		}
	}
	if count == 0 {
		return "", nil
	}
	return `<g shape-rendering="crispEdges">` + out.String() + `</g>`, nil
}

// The data-pose names are the existing face_svg protocol, not new mood names.
var pixelPoses = [...]string{"idle", "blink", "listen", "think", "talk", "sleep"}

func compileCompanionSprite(spec companionSprite) (string, error) {
	p, err := spriteColors(spec.Palette)
	if err != nil {
		return "", err
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
				rows = autoSpritePose(rows, pixelPoses[i], p)
			}
			for key, row := range edits {
				j, err := strconv.Atoi(key)
				if err != nil || j < 0 || j >= h || strconv.Itoa(j) != key {
					return "", fmt.Errorf("sprite %s: invalid row index %q", pixelPoses[i], key)
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
	names := [maskCount]string{"", "eyes", "mouth", "nose", "whiskers", "cheeks", "forehead"}
	var masks [len(pixelPoses)][maskCount][]string
	for pose, rows := range poses {
		for i := range maskCount {
			masks[pose][i] = make([]string, h)
		}
		for y, row := range rows {
			var line [maskCount][24]byte
			for x := range w {
				for i := range maskCount {
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
					if part == 4 {
						line[0][x] = 'p'
					}
				}
			}
			for i := range maskCount {
				masks[pose][i][y] = string(line[i][:w])
			}
		}
	}
	var fixed strings.Builder
	var body [len(pixelPoses)]strings.Builder
	scale := 3 * (192 / max(w, h) / 3)
	x, y := (360-w*scale)/2, (360-h*scale)/2
	if spec.partTags {
		y -= scale // Clear the starter's persistent greeting below the chin.
	}
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
			art, err := spriteRects(rows, p, x, y, scale)
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
		if strings.Count(fixed.String(), "<rect ")+strings.Count(body[i].String(), "<rect ") > maxSpriteRuns {
			return "", fmt.Errorf("sprite %s exceeds %d rectangle runs", pose, maxSpriteRuns)
		}
		fmt.Fprintf(&out, `<g data-pose="%s">%s</g>`, pose, body[i].String())
	}
	out.WriteString("</svg>")
	raw := out.String()
	if _, err := parseFaceSVG(raw); err != nil {
		return "", err
	}
	return raw, nil
}

func autoSpritePose(rows []string, pose string, p map[string]string) []string {
	// Prefer the existing eye and mouth region. Keep edits off the silhouette
	// where possible; a sprite with no face still gets a small color change.
	h, w := len(rows), len(rows[0])
	var changed [24][24]bool
	edit := func(x, y int, key byte) {
		best, col, row := 1<<30, -1, -1
		for j, line := range rows {
			for i := range len(line) {
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
			rows[row] = rows[row][:col] + string(key) + rows[row][col+1:]
		}
	}
	switch pose {
	case "blink":
		edit(w/3, h/2, 'p')
		edit(w-1-w/3, h/2, 'p')
	case "listen":
		edit(w/3, h/2, 'w')
		edit(w-1-w/3, h/2, 'w')
	case "think":
		edit(w-1-w/3, h/2, 'l')
	case "talk":
		edit(w/2-1, h*3/4, 'k')
		edit(w/2, h*3/4, 'r')
	case "sleep":
		edit(w/3, h/2, 'p')
		edit(w-1-w/3, h/2, 'p')
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
		"..kk........kk..",
		"..kpk......kpk..",
		"..kcokkkkkkock..",
		"..kccpoppopcck..",
		"..kppppooppppk..",
		".kpppwwwwwwpppk.",
		".kppwkwwwwkwppk.",
		".kppkmkwwkmkppk.",
		".kppkkkwwkkkppk.",
		"kbpccwwrrwwccpbk",
		".kbpcwwwwwwcpbk.",
		".kppwwkwwkwwppk.",
		"..kkppwkkwppkk..",
		"....kkkkkkkk....",
		"................",
	},
	Blink: map[string]string{
		"7": ".kppwwwwwwwwppk.", "8": ".kppkkkwwkkkppk.", "9": ".kppwwwwwwwwppk.",
	},
	Talk: map[string]string{
		"11": ".kbpcwwkkwwcpbk.", "12": ".kppwwkrrkwwppk.", "13": "..kkppwkkwppkk..",
	},
	Think: map[string]string{
		"7": ".kppwkwwwwwwppk.", "8": ".kppkmkwwkkkppk.", "9": ".kppkkkwwwwwppk.",
		"12": ".kppwwwkkwwwppk.", "13": "..kkppwwwwppkk..",
	},
	Sleep: map[string]string{
		"0": ".............lll", "1": "..kk........kkl.", "2": "..kpk......kpkll",
		"7": ".kppwwwwwwwwppk.", "8": ".kppkwwwwwwkppk.", "9": ".kppwkkwwkkwppk.",
		"12": ".kppwwwkkwwwppk.", "13": "..kkppwwwwppkk..",
	},
	listen: map[string]string{
		"7": ".kppwkkwwkkwppk.", "12": ".kppwwwkkwwwppk.", "13": "..kkppwwwwppkk..",
	},
	partTags: true,
}

func starterPart(x, y int, c byte) int {
	switch {
	case y >= 7 && y <= 9 && (x >= 4 && x <= 6 || x >= 9 && x <= 11) && (c == 'k' || c == 'm'):
		return 1
	case y >= 11 && y <= 13 && x >= 6 && x <= 9 && (c == 'k' || c == 'r'):
		return 2
	case y == 10 && c == 'r':
		return 3
	case c == 'b':
		return 4
	case y >= 10 && c == 'c':
		return 5
	case y <= 5 && x >= 5 && x <= 10 && c == 'o':
		return 6
	}
	return 0
}
