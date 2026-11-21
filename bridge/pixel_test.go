package main

import (
	"image"
	imagecolor "image/color"
	"image/draw"
	"slices"
	"strings"
	"testing"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

func TestSpriteRuns(t *testing.T) {
	art, err := compileSprite([]string{"ppXX", ".kk."}, map[string]string{"X": "#f5b895"}, 6, 9, 3)
	if err != nil || strings.Count(art, "<rect ") != 2 ||
		!strings.Contains(art, `<rect x="6" y="9" width="12" height="3" fill="#F5B895"/>`) ||
		!strings.Contains(art, `shape-rendering="crispEdges"`) {
		t.Fatalf("%s: %v", art, err)
	}
	checkerboard := make([]string, 24)
	for i := range checkerboard {
		checkerboard[i] = strings.Repeat("kw", 12)
	}
	for _, spec := range []companionSprite{
		{Rows: []string{"pp"}},
		{Rows: []string{"pp", "p"}},
		{Rows: []string{"pp", "p?"}},
		{Rows: []string{"pp", "pp"}, Palette: map[string]string{"p": "red"}},
		{Rows: []string{"pp", "pp"}, Blink: map[string]string{"01": "kk"}},
		{Rows: []string{"pp", "pp"}, Talk: map[string]string{"0": "k"}},
		{Rows: checkerboard},
	} {
		if _, err := compileCompanionSprite(spec); err == nil {
			t.Errorf("accepted malformed sprite %+v", spec)
		}
	}
}

func TestDirectRectPainterOrder(t *testing.T) {
	raw := svgStart + `
		<rect width="360" height="360" fill="#FAF6F0"/>
		<defs><linearGradient id="tone"><stop offset="0" stop-color="#C4B5FD"/><stop offset="1" stop-color="#5EEAD4"/></linearGradient></defs>
		<g transform="translate(12 30)">
		  <rect x="60" y="60" width="162" height="162" fill="#F5B895"/>
		  <circle cx="120" cy="120" r="45" fill="url(#tone)"/>
		  <rect x="90" y="90" width="60" height="42" fill="#4ADE80"/>
		  <rect x="93.5" y="97" width="24" height="22" rx="4" fill="#C4B5FD"/>
		  <g opacity="0.5"><rect x="114" y="120" width="36" height="30" fill="#1A1722"/></g>
		  <rect transform="rotate(45 145 144)" x="132" y="135" width="21" height="21" fill="#5EEAD4"/>
		  <rect x="132" y="144" width="36" height="24" fill="#7DD3FC"/>
		  <text x="129" y="170" font-size="19">A&amp;B</text>
		  <rect x="120" y="168" width="75" height="9" fill="#1A1722"/>
		</g>
		<rect x="-6" y="300" width="24" height="9" fill="#9A5B25"/>
	</svg>`
	scene, err := prepareScene(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []screenGeometry{roundScreen, {320, 240, "rect"}} {
		side := min(g.width, g.height)
		x, y := float64(g.width-side)/2, float64(g.height-side)/2
		got, err := scene.raster(g.width, g.height, x, y, side, imagecolor.Black, nil)
		if err != nil {
			t.Fatal(err)
		}
		ref := image.NewRGBA(got.Bounds())
		draw.Draw(ref, ref.Bounds(), image.NewUniform(imagecolor.Black), image.Point{}, draw.Src)
		icon, err := oksvg.ReadIconStream(strings.NewReader(scene.svg), oksvg.StrictErrorMode)
		if err != nil {
			t.Fatal(err)
		}
		icon.SetTarget(x, y, float64(side), float64(side))
		scan := screenScanner{rasterx.NewScannerGV(g.width, g.height, ref, ref.Bounds()), ref}
		icon.Draw(rasterx.NewDasher(g.width, g.height, scan), 1)
		if !slices.Equal(got.Pix, ref.Pix) {
			for i, v := range ref.Pix {
				if got.Pix[i] != v {
					p := image.Pt((i/4)%g.width, (i/4)/g.width)
					t.Fatalf("%v at %v: fast %v, vector %v", g, p, got.RGBAAt(p.X, p.Y), ref.RGBAAt(p.X, p.Y))
				}
			}
		}
	}
}
