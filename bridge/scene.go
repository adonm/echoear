package main

import (
	"fmt"
	"html"
	"strconv"
	"time"
)

const catInk = "#1A1926"

type sceneAction struct{ kind, value string }

type sceneView struct {
	bg, title, art string
	actions        map[string]sceneAction
	duringTurn     bool // Timer notices may be dismissed even while speaking.
	intercom       *intercomCard
	call           uint32
	menu           *menuContent
}

func (s *sceneView) hasIntercom() bool { return s != nil && s.intercom != nil }

func (s sceneView) svg(face string) string {
	return svgStart + `<rect width="360" height="360" fill="` + s.bg + `"/>` + face + s.art + `</svg>`
}

func svgRect(x, y, w, h, r int, fill, stroke string, sw int) string {
	return fmt.Sprintf(`<rect x="%d" y="%d" width="%d" height="%d" rx="%d" fill="%s" stroke="%s" stroke-width="%d"/>`,
		x, y, w, h, r, fill, stroke, sw)
}

func svgCircle(x, y, r int, fill string) string {
	return fmt.Sprintf(`<circle cx="%d" cy="%d" r="%d" fill="%s"/>`, x, y, r, fill)
}

func svgText(x, y, size int, text, tint, font string) string {
	return fmt.Sprintf(`<text x="%d" y="%d" fill="%s" text-anchor="middle" font-size="%d" %s>%s</text>`,
		x, y, tint, size, font, html.EscapeString(text))
}

func svgLabel(x, y, size int, text, tint string) string {
	return svgText(x, y, size, text, tint, `font-weight="bold"`)
}

func (s *sceneView) button(id, name string, a sceneAction, x, y, w, h int, art string) {
	if s.actions == nil {
		s.actions = make(map[string]sceneAction)
	}
	s.actions[id] = a
	s.art += fmt.Sprintf(`<g data-id="%s" data-hit="%d,%d,%d,%d" aria-label="%s">%s</g>`, id, x, y, w, h, html.EscapeString(name), art)
}

type catPose struct {
	mood        string
	blink       bool
	dot         int
	stopTaps    int
	canContinue bool
	battery     int
	charge      bool
	volume      int
	meter       bool
}

func catChrome(cat catState, pose catPose) sceneView {
	s := sceneView{bg: cat.FaceBG}
	ringColor, ringWidth := catInk, 3
	switch pose.mood {
	case "listening":
		ringColor, ringWidth = "#FF6978", 6+pose.dot
	case "thinking":
		ringColor, ringWidth = "#F59E0B", 5
	case "speaking":
		ringColor, ringWidth = "#0284C7", 5
	}
	ring := fmt.Sprintf(`<circle cx="180" cy="180" r="172" fill="none" stroke="%s" stroke-width="%d"/>`, ringColor, ringWidth)
	if pose.mood == "thinking" || pose.mood == "speaking" {
		s.art += ring
	} else {
		s.button("cat-ring", "Talk", sceneAction{"listen", ""}, 8, 8, 344, 344, ring)
	}
	if pose.battery >= 0 && pose.mood != "listening" && pose.mood != "thinking" {
		s.art += svgRect(258, 46, 6, 7, 2, catInk, "none", 0) + svgRect(229, 41, 31, 17, 5, "#FFFAEE", catInk, 2)
		if pose.battery > 0 {
			batteryColor := "#39D1A0"
			switch {
			case !pose.charge && pose.battery < 10:
				batteryColor = "#FF5964"
			case !pose.charge && pose.battery < 20:
				batteryColor = "#FACC15"
			}
			s.art += svgRect(232, 44, max(1, pose.battery*25/100), 11, 2, batteryColor, "none", 0)
		}
		if pose.charge {
			s.art += svgLabel(246, 53, 14, "+", catInk)
		}
	}
	if pose.meter {
		s.art += svgRect(114, 246, 132, 33, 16, "#FFFAEE", catInk, 2) +
			svgRect(123, 255, max(1, pose.volume*114/100), 15, 7, cat.AccentColor, "none", 0) +
			svgLabel(180, 267, 14, strconv.Itoa(pose.volume)+"%", catInk)
	}
	if !cat.Configured && pose.mood == "idle" && !pose.meter {
		s.art += svgLabel(180, 267, 14, "Say hi to design me!", "#475569")
	}
	switch pose.mood {
	case "", "idle":
		s.control(bounds{120, 292, 120, 48}, actionTile("menu", "Menu", "menu", palette{"#202538", "#FFFFFF"}))
	case "listening":
		s.art += svgRect(103, 30, 154, 40, 20, "#202538", "none", 0) +
			svgCircle(124, 50, 5, "#FF6978") + svgLabel(187, 56, 18, "Listening…", "#FFFFFF")
		s.control(bounds{56, 284, 114, 52}, actionTile("rec-cancel", "Cancel", "cancel", dangerPalette))
		s.control(bounds{190, 284, 114, 52}, actionTile("rec-send", "Send", "check", answerPalette))
	case "thinking":
		// Pulse just the top arc so native displays only send a small dirty rectangle.
		s.art += fmt.Sprintf(`<path d="M137 13.5 A172 172 0 0 1 223 13.5" fill="none" stroke="#FCD34D" stroke-width="%d" stroke-linecap="round"/>`, 5+pose.dot) +
			svgRect(103, 30, 154, 40, 20, "#202538", "none", 0) +
			svgLabel(197, 56, 18, "Thinking", "#FFFFFF")
		for i := range 3 {
			radius, color := 3, "#B47A28"
			if i == pose.dot {
				radius, color = 5, "#F59E0B"
			}
			s.art += svgCircle(124+12*i, 50, radius, color)
		}
		if pose.canContinue {
			s.control(bounds{120, 244, 120, 42}, actionTile("rec-continue", "Say more", "", answerPalette))
		}
		s.stopControl(pose.stopTaps)
	case "speaking":
		s.stopControl(0)
	}
	return s
}

func volumeLabel(auto bool, level int) string {
	value := strconv.Itoa(level) + "%"
	if auto {
		return "Auto · " + value
	}
	return value
}

func messageScene(title, heading, text string, cat catState) sceneView {
	s := sceneView{bg: "#251C3B", title: title}
	s.art = svgRect(42, 84, 276, 204, 29, "#FFFAEE", catInk, 4) +
		svgCircle(180, 83, 17, cat.AccentColor) + svgLabel(180, 129, 28, heading, catInk)
	for i, line := range cardLines(text) {
		s.art += svgLabel(180, 159+i*23, 14, line, catInk)
	}
	return s
}

type intercomCard struct {
	sender peer
	text   string
	cat    catState
}

func (card intercomCard) scene(canCall bool) sceneView {
	sender, text, cat := card.sender, card.text, card.cat
	if text == "" {
		text = "Voice message"
	}
	title := "From " + sender.Character
	s := messageScene(title, truncate(title+"!", 25), text, cat)
	s.intercom = &card
	reply := tile{id: "reply", label: "Reply", action: sceneAction{"reply", sender.DeviceID}, colors: palette{cat.AccentColor, menuPeerInk(cat.AccentColor)}}
	done := tile{id: "done", label: "Done", action: sceneAction{"cat", ""}, colors: palette{cat.FaceBG, menuPeerInk(cat.FaceBG)}}
	if canCall {
		s.control(bounds{43, 218, 78, 55}, tile{id: "call", label: "Call", action: sceneAction{"call-peer", sender.DeviceID}, colors: answerPalette})
		s.control(bounds{141, 218, 78, 55}, reply)
		s.control(bounds{239, 218, 78, 55}, done)
	} else {
		s.control(bounds{85, 218, 85, 55}, reply)
		s.control(bounds{190, 218, 85, 55}, done)
	}
	return s
}

func noticeScene(title, message string) sceneView {
	cat := blankCat()
	s := messageScene(title, title, message, cat)
	s.control(bounds{139, 300, 82, 32}, actionTile("cat", "Cat", "", palette{cat.FaceBG, catInk}))
	return s
}

func timerScene(message string) sceneView {
	cat := blankCat()
	s := messageScene("Time's up!", "Time's up!", message, cat)
	s.duringTurn = true
	s.control(bounds{139, 300, 82, 32}, tile{id: "cat", label: "OK", action: sceneAction{"dismiss", ""}, colors: palette{cat.FaceBG, catInk}})
	return s
}

func poseFrame(now, since, idle time.Time, mood string, pct, volume int, charge, meter bool) catPose {
	p := catPose{mood: mood, battery: pct, charge: charge, volume: volume, meter: meter}
	if mood == "idle" || mood == "listening" {
		p.blink = now.Sub(idle)%(3500*time.Millisecond) >= 3350*time.Millisecond
	}
	switch mood {
	case "listening":
		p.dot = int(now.Sub(since)/(300*time.Millisecond)) % 3
	case "thinking":
		p.dot = int(now.Sub(since)/(360*time.Millisecond)) % 3
	}
	return p
}
