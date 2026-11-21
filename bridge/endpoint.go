package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"time"
	"unicode"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

const (
	endpointFrame = 20 * time.Millisecond
	endpointBytes = sampleRate / 50 * 2
	endpointLimit = 60 * time.Second
	endpointIdle  = 7 * time.Second
	vadTail       = 320 * time.Millisecond
	sileroSHA256  = "6b99cbfd39246b6706f98ec13c7c50c6b299181f2474fa05cbc8046acc274396"
)

// The native model is configured locally; do not download anything at startup.
func checkEndpointModel(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return errors.New("cannot read ECHOEAR_SILERO_VAD_MODEL")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != sileroSHA256 {
		return errors.New("ECHOEAR_SILERO_VAD_MODEL must be the pinned Silero v5 model")
	}
	return nil
}

type endpointVAD interface {
	detect([]byte) bool
	close()
}

type sileroVAD struct {
	model *sherpa.VoiceActivityDetector
	frame [sampleRate / 50]float32
}

func newSileroVAD(path string) endpointVAD {
	model := sherpa.NewVoiceActivityDetector(&sherpa.VadModelConfig{
		SileroVad: sherpa.SileroVadModelConfig{
			Model: path, Threshold: 0.5, WindowSize: 512,
			MinSilenceDuration: float32(vadTail.Seconds()), MinSpeechDuration: 0.12, MaxSpeechDuration: 60,
		},
		SampleRate: sampleRate, Provider: "cpu", NumThreads: 1,
	}, 10)
	if model == nil {
		return nil
	}
	return &sileroVAD{model: model}
}

func (s *sileroVAD) detect(pcm []byte) bool {
	for i := range s.frame {
		s.frame[i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	s.model.AcceptWaveform(s.frame[:])
	voice := s.model.IsSpeech()
	for !s.model.IsEmpty() {
		s.model.Pop()
	}
	return voice
}

func (s *sileroVAD) close() { sherpa.DeleteVoiceActivityDetector(s.model) }

type endpointDecision uint8

const (
	endpointWait endpointDecision = iota
	endpointCommit
	endpointDiscard
)

type endpointPolicy struct {
	voiced  bool
	inVoice bool
	last    time.Duration // Estimated acoustic end (Silero includes a tail).
}

func (p *endpointPolicy) step(at time.Duration, speech bool, partial string) endpointDecision {
	if speech {
		p.voiced, p.inVoice = true, true
		p.last = max(0, at-vadTail)
	} else {
		p.inVoice = false
	}
	if at >= endpointLimit || !p.voiced && at >= endpointIdle {
		if p.voiced {
			return endpointCommit
		}
		return endpointDiscard
	}
	if p.voiced && !p.inVoice && at-p.last >= endpointSilence(partial) {
		return endpointCommit
	}
	return endpointWait
}

func endpointSilence(text string) time.Duration {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	const patient = 4500 * time.Millisecond
	if len(words) < 3 {
		return patient // No useful partial, or not enough context.
	}
	switch words[len(words)-1] {
	case "a", "an", "the", "and", "then", "or", "but", "so", "because", "when", "if",
		"to", "of", "in", "on", "with", "for", "about", "that", "who", "which", "named", "called":
		return patient
	}
	// A syntactically possible request ("can you draw a dragon") is not
	// necessarily finished. Fast-path only a narrow, fully specified command.
	switch words[len(words)-1] {
	case "second", "seconds", "minute", "minutes", "hour", "hours":
		text := strings.Join(words, " ")
		for _, prefix := range []string{"could you ", "can you ", "please "} {
			text = strings.TrimPrefix(text, prefix)
		}
		if len(words) >= 6 && (strings.HasPrefix(text, "start a timer for ") || strings.HasPrefix(text, "set a timer for ")) {
			return 900 * time.Millisecond
		}
	}
	return patient
}

// One capture worker owns this accumulator. Keep the original clip for the
// same whole-clip Hex fallback even when partial or VAD service fails.
type utterance struct {
	pcm    []byte
	scan   int
	policy endpointPolicy
}

func (u *utterance) add(pcm []byte, vad endpointVAD, partial string) endpointDecision {
	const limit = sampleRate * 2 * int(endpointLimit/time.Second)
	u.pcm = append(u.pcm, pcm[:min(len(pcm), limit-len(u.pcm))]...)
	decision := endpointWait
	for vad != nil && u.scan+endpointBytes <= len(u.pcm) {
		frame := u.pcm[u.scan : u.scan+endpointBytes]
		u.scan += endpointBytes
		// A later voiced frame in the same packet can revoke an endpoint.
		decision = u.policy.step(time.Duration(u.scan/endpointBytes)*endpointFrame, vad.detect(frame), partial)
	}
	if len(u.pcm) == limit && (vad == nil || u.policy.voiced) {
		return endpointCommit
	}
	return decision
}

func (u *utterance) clip() []byte {
	if len(bytes.Trim(u.pcm, "\x00")) == 0 {
		return nil
	}
	return u.pcm
}
