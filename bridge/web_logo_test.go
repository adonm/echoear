package main

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestLarkCrestAsset(t *testing.T) {
	gap := regexp.MustCompile(`>\s+<`)
	pixelArt, err := compileSprite(larkLogoSprite, nil, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/app/lark.svg", "/favicon.svg"} {
		w := httptest.NewRecorder()
		if !serveApp(w, path) || w.Code != 200 || w.Header().Get("Content-Type") != "image/svg+xml" ||
			!strings.Contains(gap.ReplaceAllString(w.Body.String(), "><"), pixelArt) {
			t.Fatalf("%s does not serve the compiled Lark emblem", path)
		}
	}
}
