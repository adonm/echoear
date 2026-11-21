package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode"
)

const (
	parakeetModel  = "parakeet-unified-en-0.6b-Q8_0.gguf"
	parakeetSHA256 = "4b50b6dd862bf6e346929aaf4f5eaacec003bfa3f56462d6c874b41ef2f38795"
)

func checkParakeetModel(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("Parakeet model missing; run setup-host.sh --stt parakeet")
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil || hex.EncodeToString(sum.Sum(nil)) != parakeetSHA256 {
		return errors.New("Parakeet model does not match the pinned GGUF")
	}
	return nil
}

func (b *bridge) decodeClip(ctx context.Context, pcm []byte) (string, error) {
	if b.parakeet != nil {
		return b.parakeet.transcribe(ctx, pcm)
	}
	return b.hex.transcribe(ctx, pcm)
}

func (b *bridge) sttEngine() string {
	if b.parakeet != nil {
		return "parakeet"
	}
	return "hex"
}

// Match the EchoEar WAV padding, then Hex's 1.5 s minimum and 400 ms
// trailing silence for Unified EN. Already-16 kHz mono audio is not resampled.
func parakeetSamples(pcm []byte) []float32 {
	const pad = len(hexSilence) / 2
	start, tail := 0, 0
	for _, b := range pcm {
		if b != 0 {
			start, tail = pad, pad
			break
		}
	}
	samples := make([]float32, max(start+len(pcm)/2+tail, sampleRate*3/2)+sampleRate*400/1000)
	for i := range len(pcm) / 2 {
		samples[start+i] = float32(int16(le.Uint16(pcm[2*i:]))) / 32768
	}
	return samples
}

// Hex strips these known subtitle hallucinations only at the very end.
var parakeetCredit = func() *regexp.Regexp {
	const sep = `[^\pL\pN]`
	credits := []string{
		"sous titrage société radio canada", "sous titrage st 501",
		"sous titres réalisés par la communauté d amara org",
		"sous titres par la communauté d amara org",
		"subtitles by the amara org community", "untertitel der amara org community",
	}
	for i, text := range credits {
		credits[i] = strings.ReplaceAll(text, " ", sep+"+")
	}
	return regexp.MustCompile(`(?i)(?:^|` + sep + `)(` + strings.Join(credits, "|") + `)` + sep + `*$`)
}()

func cleanParakeetText(text string) string {
	for {
		match := parakeetCredit.FindStringSubmatchIndex(text)
		if match == nil {
			return strings.TrimSpace(text)
		}
		text = strings.TrimRightFunc(text[:match[2]], func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune(",;:-–—", r)
		})
	}
}
