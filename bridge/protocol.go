package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
)

const (
	jsonType   = "application/json"
	pcmType    = "audio/pcm"
	wavType    = "audio/wav"
	maxJSON    = 16 << 10
	maxScreen  = 6000
	maxMessage = 400
	maxPeerPCM = 1536 << 10
)

type apiError struct {
	status int
	text   string
}

func (e *apiError) Error() string               { return e.text }
func problem(status int, text string) *apiError { return &apiError{status, text} }

type object = map[string]any

func decodeObject(raw []byte) (object, error) {
	if !json.Valid(raw) {
		return nil, problem(400, "Invalid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj object
	if dec.Decode(&obj) != nil || obj == nil {
		return nil, problem(400, "Expected a JSON object")
	}
	return obj, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func truncate(s string, n int) string {
	for i := range s {
		if n == 0 {
			return s[:i]
		}
		n--
	}
	return s
}

func shortText(v any, limit int, plain bool) string {
	s := str(v)
	if plain {
		s = plainText(s)
	}
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	return truncate(strings.TrimSpace(s), limit)
}

var (
	hexColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)
	targetRE = regexp.MustCompile(`^[\pL\pN_ .'’-]{1,64}$`)
	fenceRE  = regexp.MustCompile("(?mis)^[ \t]*```echoear-(screen|message)[ \t]*(?:\r?\n|\\z)(.*?)(^[ \t]*```[ \t]*\r?$|\\z)")
	fenced   = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*(?:\n|\\z).*?(^[ \t]*```[ \t]*\r?$|\\z)")
)

func color(v any) string {
	if s := str(v); hexColor.MatchString(s) {
		return strings.ToUpper(s)
	}
	return ""
}

var shapeFields = map[string][2][]string{
	"circle": {{"x", "y", "r"}, {"w"}},
	"rect":   {{"x", "y", "w", "h"}, {"r"}},
	"line":   {{"x1", "y1", "x2", "y2"}, {"w"}},
	"tri":    {{"x1", "y1", "x2", "y2", "x3", "y3"}, {"w"}},
	"arc":    {{"x", "y", "r", "a1", "a2"}, {"w"}},
	"text":   {{"x", "y"}, {}},
	"btn":    {{"x", "y", "w", "h"}, {"r"}},
}

func coordinate(v any, limit float64) (int, bool) {
	number, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	n, err := number.Float64()
	if err != nil && strings.ContainsAny(string(number), ".eE") || math.IsNaN(n) {
		return 0, false
	}
	return int(min(limit, max(0, n))), true
}

func sanitizeShape(v any) object {
	item, ok := v.(object)
	if !ok {
		return nil
	}
	kind, tint := str(item["t"]), color(item["c"])
	fields, ok := shapeFields[kind]
	if !ok || tint == "" {
		return nil
	}
	out := object{"t": kind, "c": tint}
	for k, keys := range fields {
		for _, key := range keys {
			limit := 359.0
			if key == "a1" || key == "a2" {
				limit = 360 // Firmware 0.6.1 accepts 360 for a full arc.
			}
			if n, ok := coordinate(item[key], limit); ok {
				out[key] = n
			} else if k == 0 {
				return nil
			}
		}
	}
	if kind == "text" || kind == "btn" {
		s := shortText(item["s"], 32, false)
		if s == "" {
			return nil
		}
		out["s"] = s
	}
	if fill, ok := item["fill"].(bool); ok && (kind == "circle" || kind == "rect" || kind == "tri") {
		out["fill"] = fill
	} else if kind == "tri" {
		out["fill"] = true
	}
	for _, key := range []string{"tap", "ask", "reply_to"} {
		limit := 120
		if key == "reply_to" {
			if kind != "btn" {
				continue
			}
			limit = 40
		}
		if s := shortText(item[key], limit, true); s != "" {
			out[key] = s
		}
	}
	return out
}

func sanitizeScreen(raw string) object {
	if len(raw) > 64<<10 {
		return nil
	}
	obj, err := decodeObject([]byte(raw))
	if err != nil {
		return nil
	}
	var items []any
	if v, ok := obj["shapes"]; ok {
		items, ok = v.([]any)
		if !ok {
			return nil
		}
	}
	out := object{}
	if title := shortText(obj["title"], 48, false); title != "" {
		out["title"] = title
	}
	if subtitle := shortText(obj["subtitle"], 80, false); subtitle != "" {
		out["subtitle"] = subtitle
	}
	if bg := color(obj["bg"]); bg != "" {
		out["bg"] = bg
	}
	shapes := make([]object, 0, min(24, len(items)))
	for _, item := range items[:min(24, len(items))] {
		if shape := sanitizeShape(item); shape != nil {
			shapes = append(shapes, shape)
		}
	}
	if len(out) == 0 && len(shapes) == 0 {
		return nil
	}
	if out["bg"] == nil {
		out["bg"] = "#0B1020"
	}
	for {
		out["shapes"] = shapes
		raw, _ := json.Marshal(out)
		if len(raw) < maxScreen {
			return out
		}
		shapes = shapes[:len(shapes)-1]
	}
}

type intercomMessage struct{ target, text string }

func messagePayload(raw string) *intercomMessage {
	if len(raw) > maxJSON {
		return nil
	}
	obj, err := decodeObject([]byte(raw))
	if err != nil {
		return nil
	}
	s, text := strings.TrimSpace(str(obj["to"])), shortText(obj["text"], maxMessage, true)
	if targetRE.MatchString(s) && text != "" {
		return &intercomMessage{s, text}
	}
	return nil
}

func extractPayloads(reply string) (string, object, *intercomMessage) {
	var screen object
	var message *intercomMessage
	reply = fenceRE.ReplaceAllStringFunc(reply, func(block string) string {
		m := fenceRE.FindStringSubmatch(block)
		if m[3] != "" {
			switch strings.ToLower(m[1]) {
			case "screen":
				if screen == nil {
					screen = sanitizeScreen(m[2])
				}
			case "message":
				if message == nil {
					message = messagePayload(m[2])
				}
			}
		}
		return ""
	})
	return strings.TrimSpace(reply), screen, message
}

func cardLines(text string) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		for word != "" {
			n := 27 - runeLen(line)
			if line != "" {
				n--
				if runeLen(word) > n {
					lines = append(lines, line)
					line = ""
					continue
				}
				line += " "
			}
			chunk := truncate(word, n)
			line += chunk
			word = word[len(chunk):]
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	if len(lines) > 3 {
		lines = lines[:3]
		if runeLen(lines[2]) >= 27 {
			if i := strings.LastIndexByte(lines[2], ' '); i >= 0 {
				lines[2] = lines[2][:i]
			} else {
				lines[2] = truncate(lines[2], 26)
			}
		}
		lines[2] += "…"
	}
	return lines
}

func messageCard(sender peer, text string) object {
	if text == "" {
		text = "Voice message"
	}
	lines := cardLines(text)
	shapes := make([]object, 0, len(lines)+1)
	for i, line := range lines {
		shapes = append(shapes, object{"t": "text", "x": 180, "y": 124 + i*25, "c": "#FFF1C8", "s": line})
	}
	shapes = append(shapes, object{"t": "btn", "x": 85, "y": 218, "w": 190, "h": 46, "r": 23, "c": "#34D399",
		"s": truncate("Reply to "+sender.Character, 32), "reply_to": sender.DeviceID})
	return object{"bg": "#1E1B3A", "title": fmt.Sprintf("From %s!", sender.Character), "shapes": shapes}
}
