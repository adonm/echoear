package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	sherpa "github.com/k2-fsa/sherpa-onnx-go/sherpa_onnx"
)

// Run explicitly: this generates synthetic speech and decodes locally on Vulkan.
// Fixtures and transcripts are held in memory only; no live mic is involved.
func TestEndpointReplay(t *testing.T) {
	path := os.Getenv("ECHOEAR_REPLAY_SILERO")
	if path == "" {
		t.Skip("set ECHOEAR_REPLAY_SILERO to run local synthetic shadow replay")
	}
	if err := checkEndpointModel(path); err != nil {
		t.Fatal(err)
	}
	kokoro := os.Getenv("ECHOEAR_REPLAY_KOKORO")
	if kokoro == "" {
		t.Fatal("set ECHOEAR_REPLAY_KOKORO to the local Kokoro model directory")
	}
	model := os.Getenv("ECHOEAR_BAKEOFF_MODEL")
	if err := checkParakeetModel(model); err != nil {
		t.Fatal(err)
	}
	parakeet, err := openParakeet(model)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(parakeet.close)
	clip := parakeet.transcribe
	speculative := os.Getenv("ECHOEAR_REPLAY_SPECULATIVE") == "1"
	tts := sherpa.NewOfflineTts(&sherpa.OfflineTtsConfig{
		Model: sherpa.OfflineTtsModelConfig{Provider: "cpu", NumThreads: 2, Kokoro: sherpa.OfflineTtsKokoroModelConfig{
			Model: filepath.Join(kokoro, "model.onnx"), Voices: filepath.Join(kokoro, "voices.bin"),
			Tokens: filepath.Join(kokoro, "tokens.txt"), DataDir: filepath.Join(kokoro, "espeak-ng-data"), Lang: "en-us", LengthScale: 1,
		}}, MaxNumSentences: 1,
	})
	if tts == nil {
		t.Fatal("Kokoro unavailable")
	}
	defer sherpa.DeleteOfflineTts(tts)
	speak := func(text string) []byte {
		a := tts.Generate(text, 2, 1)
		if a == nil || a.SampleRate != 24000 {
			t.Fatal("fixture synthesis failed")
		}
		var pcm []byte
		for k := 0; k+2 < len(a.Samples); k += 3 {
			pcm = le.AppendUint16(pcm, uint16(int16(a.Samples[k]*32767)))
			pcm = le.AppendUint16(pcm, uint16(int16((a.Samples[k+1]+a.Samples[k+2])*16383)))
		}
		// Strip Kokoro's variable silent tail before adding the scripted gaps.
		const tick = endpointBytes / 2
		seemsVoiced := func(p []byte) bool {
			var energy int64
			for i := 0; i < len(p); i += 2 {
				x := int64(int16(le.Uint16(p[i:])))
				energy += x * x
			}
			return energy > int64(len(p)/2)*240*240
		}
		start, end := 0, 0
		for i := 0; i+tick <= len(pcm); i += tick {
			if seemsVoiced(pcm[i : i+tick]) {
				if end == 0 {
					start = max(0, i-3*tick)
				}
				end = i + tick
			}
		}
		if end == 0 {
			t.Fatal("empty fixture")
		}
		return pcm[start:end]
	}
	silence := func(ms int) []byte { return make([]byte, sampleRate/1000*2*ms) }
	type marker struct {
		kind      string
		spellings []string
	}
	type fixture struct {
		before, after string
		gap           int
		targets       []marker
	}
	frogLead := "Once upon a time there was a little frog, and then"
	frogTail := "it made a house under a purple mushroom."
	fixtures := []fixture{
		{"Start a timer for ninety seconds.", "", 0, []marker{{"number", []string{"ninety", "90"}}}},
		{"Tell me a joke about Saoirse.", "", 0, []marker{{"name", []string{"saoirse"}}}},
		{frogLead, frogTail, 700, nil}, {frogLead, frogTail, 1700, nil},
		{frogLead, frogTail, 3200, nil}, {frogLead, frogTail, 4000, nil},
		{"Could you set a timer for", "seventeen minutes?", 2600, []marker{{"number", []string{"seventeen", "17"}}}},
		{"Tell me a joke about", "Siobhan and thirty-two spiders.", 3600, []marker{{"name", []string{"siobhan"}}, {"number", []string{"thirty two", "32"}}}},
		{"Can you draw a dragon", "with blue wings and little stars?", 1700, nil},
	}
	type score struct {
		early, missed    int
		delay, finalized []int
	}
	type accuracy struct{ errors, count, failures int }
	accuracyByPath := map[string]map[string]*accuracy{}
	recordText := func(path string, markers []marker, text string, err error) {
		if accuracyByPath[path] == nil {
			accuracyByPath[path] = map[string]*accuracy{"name": {}, "number": {}}
		}
		tokens := " " + strings.Join(replayWords(text), " ") + " "
		for _, m := range markers {
			s := accuracyByPath[path][m.kind]
			if err != nil {
				s.failures++
			} else {
				s.count++
				if !slices.ContainsFunc(m.spellings, func(term string) bool { return strings.Contains(tokens, " "+term+" ") }) {
					s.errors++
				}
			}
		}
	}
	scores := map[string]*score{}
	record := func(label string, at, end int) {
		if scores[label] == nil {
			scores[label] = new(score)
		}
		s := scores[label]
		switch {
		case at < 0:
			s.missed++
		case at < end:
			s.early++
		default:
			s.delay = append(s.delay, at-end)
		}
	}
	recordFinal := func(label string, at, end int, start time.Time, err error) {
		if err == nil && at >= end {
			s := scores[label]
			s.finalized = append(s.finalized, at-end+int(time.Since(start).Milliseconds()))
		}
	}
	var fixtureID, failed int
	for _, f := range fixtures {
		fixtureID++
		pcm := append(silence(250), speak(f.before)...)
		split := len(pcm) / (sampleRate / 1000 * 2)
		if f.after != "" {
			pcm = append(pcm, silence(f.gap)...)
			pcm = append(pcm, speak(f.after)...)
		}
		trueEnd := len(pcm) / (sampleRate / 1000 * 2)
		pcm = append(pcm, silence(5200)...)
		vad := newSileroVAD(path)
		if vad == nil {
			t.Fatal("Silero unavailable")
		}
		var silero utterance
		plainAt, hintAt := -1, -1
		ctx, stop := context.WithTimeout(t.Context(), 25*time.Second)
		var shaped endpointPolicy
		var probes [2]replayPauseProbe
		pauseMS := [...]int{500, 700}
		labels := [...]string{"silero-speculative-500", "silero-speculative-700"}
		for i := endpointBytes; i <= len(pcm); i += endpointBytes {
			at := i / (sampleRate / 1000 * 2)
			if plainAt < 0 && silero.add(pcm[i-endpointBytes:i], vad, "") == endpointCommit {
				plainAt = at
			} else if plainAt >= 0 {
				_ = silero.add(pcm[i-endpointBytes:i], vad, "")
			}
			// Compare against an optimistic text-hint timing upper bound.
			hint := f.before
			if at >= trueEnd && trueEnd > split {
				hint += " " + f.after
			}
			if hintAt < 0 && shaped.step(time.Duration(at)*time.Millisecond, silero.policy.inVoice, hint) == endpointCommit {
				hintAt = at
			}
			if speculative {
				for j := range probes {
					probe := &probes[j]
					if probe.commitAt != 0 {
						continue
					}
					policy := silero.policy
					if policy.inVoice {
						*probe = replayPauseProbe{}
					} else if policy.voiced && probe.readyAt == 0 && at-int(policy.last.Milliseconds()) >= pauseMS[j] {
						start := time.Now()
						probe.text, probe.err = clip(ctx, pcm[:i])
						probe.readyAt = at + int((time.Since(start) + time.Millisecond - 1).Milliseconds())
					}
					var text string
					if probe.err == nil && at >= probe.readyAt {
						text = probe.text
					}
					if policy.step(time.Duration(at)*time.Millisecond, policy.inVoice, text) == endpointCommit {
						probe.commitAt = at
						if text == "" {
							start := time.Now()
							probe.text, probe.err = clip(ctx, pcm[:i])
							probe.readyAt = at + int(time.Since(start).Milliseconds())
						}
					}
				}
			}
		}
		vad.close()
		browserAt, boardAt := replayLegacy(pcm, false), replayLegacy(pcm, true)
		record("browser-rms", browserAt, trueEnd)
		record("board-rms", boardAt, trueEnd)
		record("silero-no-text", plainAt, trueEnd)
		if speculative {
			for j, probe := range probes {
				at := probe.commitAt
				if at == 0 {
					at = -1
				}
				record(labels[j], at, trueEnd)
				if at >= trueEnd && probe.err == nil {
					scores[labels[j]].finalized = append(scores[labels[j]].finalized, max(at, probe.readyAt)-trueEnd)
				}
				recordText(labels[j], f.targets, probe.text, probe.err)
			}
			t.Logf("fixture=%d speculative_commit_ms=%d/%d", fixtureID, probes[0].commitAt, probes[1].commitAt)
		}
		record("silero-oracle-text (not live)", hintAt, trueEnd)
		end := len(pcm)
		if plainAt >= 0 {
			end = min(end, plainAt*32)
		}
		startFinal := time.Now()
		text, err := clip(ctx, pcm[:end])
		recordFinal("silero-no-text", plainAt, trueEnd, startFinal, err)
		recordText("silero + whole clip", f.targets, text, err)
		for _, tc := range []struct {
			path string
			stop int
		}{{"board-rms", boardAt}, {"browser-rms", browserAt}} {
			if tc.stop >= 0 {
				start := time.Now()
				got, sttErr := clip(ctx, pcm[:min(len(pcm), tc.stop*32)])
				recordFinal(tc.path, tc.stop, trueEnd, start, sttErr)
				recordText(tc.path+" + clip", f.targets, got, sttErr)
			}
		}
		stop()
		if err != nil {
			failed++
		}
		t.Logf("fixture=%d gap_ms=%d true_end_ms=%d board=%d browser=%d silero=%d hinted=%d STT_ok=%t",
			fixtureID, f.gap, trueEnd, boardAt, browserAt, plainAt, hintAt, err == nil)
	}
	for method, s := range scores {
		t.Logf("%s premature=%d/%d missed=%d end_to_endpoint_ms=%s end_to_final_ms=%s", method, s.early, len(fixtures), s.missed,
			replayPercentiles(s.delay), replayPercentiles(s.finalized))
	}
	for path, acc := range accuracyByPath {
		n, q := acc["name"], acc["number"]
		t.Logf("%s name_errors=%d/%d number_errors=%d/%d unscored_markers=%d", path, n.errors, n.count, q.errors, q.count, n.failures+q.failures)
	}
	if failed != 0 {
		t.Errorf("Silero path STT failures: %d", failed)
	}
}

// A decode runs once per pause. Its result becomes visible on the replay's
// audio clock after the measured wall duration; a resumed voice invalidates it.
type replayPauseProbe struct {
	text              string
	err               error
	readyAt, commitAt int
}

func replayWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
}

func replayPercentiles(v []int) string {
	if len(v) == 0 {
		return "n/a"
	}
	slices.Sort(v)
	return fmt.Sprintf("n=%d p50=%d p95=%d", len(v), v[(len(v)*50+99)/100-1], v[(len(v)*95+99)/100-1])
}

// Reproduce firmware/main/vad.c or web/mic.js using the replay audio clock.
func replayLegacy(pcm []byte, board bool) int {
	tick, calibration, floorN, startWait, short, long, longVoice := 20, 10, 3, 400, 75, 150, 200
	if board {
		tick, calibration, floorN, startWait, short, long, longVoice = 10, 20, 4, 700, 150, 300, 400
	}
	size := sampleRate * tick / 1000 * 2
	var quiet []float64
	var threshold float64
	voice, streak, silence := 0, 0, 0
	for i, frames := size, 1; i <= len(pcm); i, frames = i+size, frames+1 {
		var sum float64
		for j := i - size; j < i; j += 2 {
			x := float64(int16(le.Uint16(pcm[j:])))
			sum += x * x
		}
		rms := math.Sqrt(sum / float64(size/2))
		hear := func(level float64) {
			if voice > 0 && level >= threshold/2 || voice == 0 && level >= threshold {
				voice++
				silence = 0
			} else if voice > 0 {
				silence++
			}
		}
		switch {
		case frames <= calibration:
			quiet = append(quiet, rms)
			if frames == calibration {
				sorted := slices.Clone(quiet)
				slices.Sort(sorted)
				var avg float64
				for _, r := range sorted[:floorN] {
					if board {
						r *= r
					}
					avg += r
				}
				avg /= float64(floorN)
				if board {
					threshold = min(2400, max(320, math.Sqrt(avg)*2.5))
				} else {
					threshold = max(327.68, avg*2.5)
					for _, r := range quiet {
						hear(r)
					}
				}
			}
		case board && voice == 0:
			if rms >= threshold {
				streak++
			} else {
				streak = 0
			}
			if streak >= 3 {
				voice = streak
			}
		default:
			hear(rms)
		}
		if frames*tick >= 60000 || voice == 0 && frames >= startWait ||
			voice < longVoice && silence >= short || voice >= longVoice && silence >= long {
			return frames * tick
		}
	}
	return -1
}
