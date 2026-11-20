package main

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
)

const (
	jsonType   = "application/json"
	pcmType    = "audio/pcm"
	wavType    = "audio/wav"
	maxJSON    = 16 << 10
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
	fenced   = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*(?:\n|\\z).*?(^[ \t]*```[ \t]*\r?$|\\z)")
)

func color(v any) string {
	if s := str(v); hexColor.MatchString(s) {
		return strings.ToUpper(s)
	}
	return ""
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
