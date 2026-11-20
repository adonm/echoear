package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var scientificPitch = regexp.MustCompile(`^([A-G])([#b]?)([3-7])$`)

func noteFrequency(pitch string) (float64, bool) {
	if pitch == "R" {
		return 0, true
	}
	if m := scientificPitch.FindStringSubmatch(pitch); m != nil {
		semitone := [...]int{9, 11, 0, 2, 4, 5, 7}[m[1][0]-'A'] + 12*int(m[3][0]-'4')
		switch m[2] {
		case "#":
			semitone++
		case "b":
			semitone--
		}
		if semitone < -12 || semitone > 36 { // C3 through C7, inclusive.
			return 0, false
		}
		return 440 * math.Exp2(float64(semitone-9)/12), true
	}
	hz, err := strconv.ParseFloat(pitch, 64)
	return hz, err == nil && hz >= 100 && hz <= 2500
}

func synthesizeMelody(notes []string) ([]byte, error) {
	if len(notes) < 1 || len(notes) > 32 {
		return nil, fmt.Errorf("expected 1–32 notes in PITCH:MS format")
	}
	type note struct {
		hz      float64
		samples int
	}
	parsed := make([]note, len(notes))
	total := 0
	for i, token := range notes {
		pitch, duration, split := strings.Cut(token, ":")
		ms, err := strconv.Atoi(duration)
		hz, ok := noteFrequency(pitch)
		if !split || !ok || err != nil || ms < 30 || ms > 2000 {
			return nil, fmt.Errorf("note %d: use C3–C7, R, or 100–2500 Hz followed by :30–2000 (milliseconds)", i+1)
		}
		total += ms
		if total > 6000 {
			return nil, fmt.Errorf("total duration must not exceed 6000 ms")
		}
		parsed[i] = note{hz, ms * sampleRate / 1000}
	}
	pcm := make([]byte, 2*total*sampleRate/1000)
	offset := 0
	for _, n := range parsed {
		if n.hz != 0 {
			step := 2 * math.Pi * n.hz / sampleRate
			for i := range n.samples {
				phase := step * float64(i)
				fade := float64(min(i, n.samples-1-i, sampleRate/100)) / (sampleRate / 100)
				sample := int16(32767 * 0.45 * fade * (0.85*math.Sin(phase) + 0.15*math.Sin(2*phase)))
				binary.LittleEndian.PutUint16(pcm[offset+2*i:], uint16(sample))
			}
		}
		offset += 2 * n.samples
	}
	return pcm, nil
}

func parseMelody(raw any, path string) ([]string, []byte, error) {
	var notes []string
	switch v := raw.(type) {
	case []any:
		if len(v) <= 32 {
			notes = make([]string, len(v))
			for i, token := range v {
				s, ok := token.(string)
				if !ok {
					return nil, nil, fieldError(fmt.Sprintf("%s[%d]", path, i), "expected PITCH:MS")
				}
				notes[i] = s
			}
		}
	case []string:
		notes = v
	}
	pcm, err := synthesizeMelody(notes)
	if err != nil {
		return nil, nil, fieldError(path, "%s", err)
	}
	return notes, pcm, nil
}

func (h *wsHub) playPCM(d *wsDevice, pcm []byte) {
	stream := newAudioStream()
	stream.append(pcm)
	stream.finish(nil)
	h.play(d, d.client.ctx, stream)
}
