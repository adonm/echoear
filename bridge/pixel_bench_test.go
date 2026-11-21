package main

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkRenderPixelScene(b *testing.B) {
	var svg strings.Builder
	svg.WriteString(svgStart + `<rect width="360" height="360" fill="#FAF6F0"/>`)
	for y := range 16 {
		for x := range 16 {
			if (x+y)%3 != 0 {
				fmt.Fprintf(&svg, `<rect x="%d" y="%d" width="12" height="12" fill="#%s"/>`, 84+x*12, 72+y*12,
					[...]string{"F5B895", "1A1722", "5EEAD4"}[(x+y/2)%3])
			}
		}
	}
	svg.WriteString(`<circle cx="180" cy="180" r="9" fill="#FDE047"/></svg>`)
	raw := svg.String()
	b.ResetTimer()
	for range b.N {
		if _, err := buildScreenUncached(raw, true, roundScreen); err != nil {
			b.Fatal(err)
		}
	}
}
