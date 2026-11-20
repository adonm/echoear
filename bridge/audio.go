package main

import (
	"encoding/binary"
	"errors"
	"math"
	"regexp"
	"strings"
	"unicode"
)

const (
	sampleRate = 16000
	maxPCM     = 4 << 20
	maxText    = 640
)

var (
	le            = binary.LittleEndian
	errInvalidPCM = errors.New("invalid synthesized audio")
)

func wavHeader(size int) (h [44]byte) {
	copy(h[:], "RIFF")
	le.PutUint32(h[4:], uint32(36+size))
	copy(h[8:], "WAVEfmt ")
	le.PutUint32(h[16:], 16)
	le.PutUint16(h[20:], 1)
	le.PutUint16(h[22:], 1)
	le.PutUint32(h[24:], sampleRate)
	le.PutUint32(h[28:], sampleRate*2)
	le.PutUint16(h[32:], 2)
	le.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	le.PutUint32(h[40:], uint32(size))
	return h
}

func WrapWAV(pcm16k []byte) []byte {
	h := wavHeader(len(pcm16k))
	return append(append(make([]byte, 0, len(h)+len(pcm16k)), h[:]...), pcm16k...)
}

func wavPCM(wav []byte) ([]byte, error) {
	format := false
	if len(wav) >= 12 && string(wav[:4]) == "RIFF" && string(wav[8:12]) == "WAVE" {
		end := int(le.Uint32(wav[4:])) + 8
		for i := 12; i+8 <= min(end, len(wav)); {
			n := int(le.Uint32(wav[i+4:]))
			start := i + 8
			if n > min(end, len(wav))-start {
				break
			}
			chunk := wav[start : start+n]
			switch string(wav[i : i+4]) {
			case "fmt ":
				format = n >= 16 && (le.Uint16(chunk) == 1 ||
					(le.Uint16(chunk) == 0xfffe && n >= 40 && string(chunk[24:40]) == "\x01\x00\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71")) &&
					le.Uint16(chunk[2:]) == 1 && le.Uint32(chunk[4:]) == sampleRate && le.Uint16(chunk[14:]) == 16
			case "data":
				if format && n >= 2 {
					return chunk[:n&^1], nil
				}
				return nil, problem(400, "Intercom requires 16 kHz mono 16-bit PCM")
			}
			i = start + n + n%2
		}
	}
	return nil, problem(400, "Intercom requires 16 kHz mono 16-bit PCM")
}

// At an output position 3*n/2, the fractional input position is either 0 or 1/2.
const sincRadius = 16

var sinc = func() (phases [2][2*sincRadius + 1]float32) {
	for p := range phases {
		var sum float32
		for i := range phases[p] {
			x := float64(i-sincRadius) - float64(p)*0.5
			y := math.Pi * x / (sincRadius + 1)
			window := 0.42 + 0.5*math.Cos(y) + 0.08*math.Cos(2*y)
			v := 0.62
			if x != 0 {
				v = math.Sin(math.Pi*v*x) / (math.Pi * x)
			}
			phases[p][i] = float32(v * window)
			sum += phases[p][i]
		}
		for i := range phases[p] {
			phases[p][i] /= sum
		}
	}
	return phases
}()

func synthPCM(raw []byte, rate uint32) ([]byte, error) {
	if len(raw)%4 != 0 || len(raw) == 0 || (rate != 24000 && rate != sampleRate) {
		return nil, errInvalidPCM
	}
	n := len(raw) / 4
	count := n
	if rate == 24000 {
		count = (n*2 + 2) / 3
	}
	if count > maxPCM/2 {
		return nil, errInvalidPCM
	}
	input := make([]float32, n+2*sincRadius+1)
	for i := range n {
		input[sincRadius+i] = math.Float32frombits(le.Uint32(raw[4*i:]))
	}
	pcm := make([]byte, count*2)
	var peak float64
	const knee, headroom = 0.75, 0.8912509381337456 - 0.75
	for i := range count {
		v := input[i+sincRadius]
		if rate == 24000 {
			center := 3 * i / 2
			samples := input[center : center+2*sincRadius+1]
			weights := &sinc[i&1]
			v = 0
			for j, sample := range samples {
				v += sample * weights[j]
			}
		}
		scaled := float64(v) * 1.8
		mag := math.Abs(scaled)
		peak = max(peak, mag)
		if mag > knee {
			mag = knee + headroom*math.Tanh((mag-knee)/headroom)
		}
		le.PutUint16(pcm[2*i:], uint16(int16(math.RoundToEven(math.Copysign(mag, scaled)*32767))))
	}
	if peak < 1e-5 || math.IsNaN(peak) || math.IsInf(peak, 0) {
		return nil, errInvalidPCM
	}
	return pcm, nil
}

func MakeIntercomChimePCM() []byte {
	return chimePCM("intercom")
}

func chimePCM(kind string) []byte {
	tones := []float64{660, 880}
	switch kind {
	case "tap":
		tones = []float64{740}
	case "think":
		tones = []float64{520, 620}
	case "celebrate":
		tones = []float64{660, 830, 990}
	}
	const length, fade, pause = sampleRate / 10, sampleRate / 80, sampleRate / 50
	pcm := make([]byte, (length*len(tones)+pause*(len(tones)-1))*2)
	for t, tone := range tones {
		for i := range length {
			envelope := min(1, float64(i)/fade, float64(length-1-i)/fade)
			value := math.RoundToEven(4600 * envelope * math.Sin(2*math.Pi*tone*float64(i)/sampleRate))
			le.PutUint16(pcm[2*(t*(length+pause)+i):], uint16(int16(value)))
		}
	}
	return pcm
}

func NormalizeVoicePCM(pcm16k []byte) []byte {
	if len(pcm16k) < 2 || len(pcm16k)%2 != 0 {
		return pcm16k
	}
	var peak int
	var power int64
	for i := 0; i < len(pcm16k); i += 2 {
		v := int(int16(le.Uint16(pcm16k[i:])))
		peak = max(peak, v, -v)
		power += int64(v * v)
	}
	if peak < 1100 || power < int64(len(pcm16k)/2)*400*400 {
		return pcm16k
	}
	gain := min(3, 26027.2982/float64(peak))
	out := make([]byte, len(pcm16k))
	for i := 0; i < len(pcm16k); i += 2 {
		v := float64(int16(le.Uint16(pcm16k[i:])))
		le.PutUint16(out[i:], uint16(int16(math.RoundToEven(v*gain))))
	}
	return out
}

func alphaNum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

func plainText(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_`#", r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

var (
	sentenceEnd = regexp.MustCompile(`[.!?]+["'”’)\]]*`)
	abbrev      = regexp.MustCompile(`(?i)\b(?:Mr|Mrs|Ms|Dr|Prof|St|vs|etc|e\.g|i\.e)\.$`)
	initials    = regexp.MustCompile(`\b(?:[A-Z]\.)+$`)
)

func sentences(text string, final, boundary bool) (out []string) {
	start, clause := 0, 0
	for _, loc := range sentenceEnd.FindAllStringIndex(text, -1) {
		a, b := loc[0], loc[1]
		if b < len(text) && text[b] != ' ' || b == len(text) && !(final || boundary) {
			continue
		}
		s := text[clause:b]
		if text[a] == '.' && (abbrev.MatchString(s) || initials.MatchString(s)) {
			continue
		}
		clause = b
		if text[a] != '.' {
			count := 0
			for _, r := range s {
				if alphaNum(r) {
					count++
				}
			}
			if count < 20 {
				continue
			}
		}
		out = append(out, strings.TrimSpace(text[start:b]))
		start = b
	}
	if s := strings.TrimSpace(text[start:]); final && s != "" {
		out = append(out, s)
	}
	return out
}

func SplitSentences(text string) []string { return sentences(plainText(text), true, true) }

type sentenceExtractor struct {
	raw     string
	emitted int
	halted  bool
}

func (s *sentenceExtractor) add(delta string) []string {
	if s.halted {
		return nil
	}
	s.raw += delta
	if len(s.raw) > 8*1024 {
		s.halted = true
		return nil
	}
	if i := strings.Index(s.raw, "```"); i != -1 {
		s.raw, s.halted = s.raw[:i], true
	}
	// A period at the end of a delta may still become a decimal or abbreviation.
	boundary := s.halted || len(s.raw) != len(strings.TrimRightFunc(s.raw, unicode.IsSpace))
	all := sentences(plainText(s.raw), false, boundary)
	if len(all) <= s.emitted {
		return nil
	}
	next := all[s.emitted:]
	s.emitted = len(all)
	return next
}
