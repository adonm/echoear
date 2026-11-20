package main

import (
	"encoding/binary"
	"errors"
	"math"
	"regexp"
	"slices"
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

func ambientRMS(pcm []byte) (float64, bool) {
	const block = sampleRate / 100 * 2
	var energy [20]int64
	if len(pcm) < len(energy)*block {
		return 0, false
	}
	for i := range energy {
		for offset := i * block; offset < (i+1)*block; offset += 2 {
			s := int64(int16(le.Uint16(pcm[offset:])))
			energy[i] += s * s
		}
	}
	slices.Sort(energy[:])
	return math.Sqrt(float64(energy[0]+energy[1]+energy[2]+energy[3]) / (4 * (block / 2))), true
}

func ambientVolume(rms float64) int {
	curve := [...]struct{ rms, level float64 }{{80, 40}, {300, 58}, {800, 78}, {1800, 100}}
	for i, p := range curve {
		if rms <= p.rms {
			if i == 0 {
				return int(p.level)
			}
			prev := curve[i-1]
			return int(math.Round(prev.level + (p.level-prev.level)*(rms-prev.rms)/(p.rms-prev.rms)))
		}
	}
	return 100
}

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
