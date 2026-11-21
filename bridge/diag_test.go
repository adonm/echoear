package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsMetricWindowAndTurns(t *testing.T) {
	b := &bridge{}
	t0 := time.Now()
	for i := 1; i <= 40; i++ {
		ctx := b.traceTurn(t.Context(), "board", "Cake", "voice", t0.Add(time.Duration(i)*time.Second))
		trace := traceFrom(ctx)
		trace.measure(diagSTT, time.Duration(i)*time.Millisecond)
		trace.measure(diagSTT, 999*time.Millisecond)
		trace.audioEnqueued(trace.start.Add(50 * time.Millisecond))
	}
	report := b.diagnosticReport()
	stt := report.Latency["stt_ms"]
	if stt.Count != 32 || *stt.Last != 40 || *stt.P50 != 24 || *stt.P95 != 39 || stt.Samples[0] != 9 {
		t.Fatalf("unexpected STT window: %+v", stt)
	}
	if report.Latency["llm_first_ms"].Last != nil || len(report.Turns) != 20 ||
		report.Turns[0].TS != t0.Add(40*time.Second).UnixMilli() || *report.Turns[19].STT != 21 ||
		*report.Turns[0].E2E != 50 || report.Turns[0].Device != "board" || report.Turns[0].Character != "Cake" {
		t.Fatalf("unexpected turn log: %+v", report.Turns)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			trace := traceFrom(b.traceTurn(t.Context(), "web", "Mint", "typed", time.Now()))
			trace.measure(diagLLMFirst, 3*time.Millisecond)
			trace.measure(diagLLMTotal, 7*time.Millisecond)
			_ = b.diagnosticReport()
		})
	}
	wg.Wait()
	if m := b.diagnosticReport().Latency["llm_total_ms"]; m.Count != 16 || *m.P95 != 7 {
		t.Fatalf("concurrent metric writes: %+v", m)
	}
}

func TestDiagnosticsProcAndShutdown(t *testing.T) {
	if got := parseProcStat("42 (cat ) server) S 0 0 0 0 0 0 0 0 0 0 12 30 0 0 0 0 0 0 567", 42); got != (procReading{42, 42, 567}) {
		t.Fatal(got)
	}
	if got := parseHostCPU("cpu 1 2 3 4 5 6 7 8 9 10\ncpu0 15"); got != (cpuReading{27, 36}) {
		t.Fatal(got)
	}
	path := filepath.Join(t.TempDir(), "echoear.db")
	for _, f := range []string{path, path + "-wal"} {
		if err := os.WriteFile(f, []byte("telemetry"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, cancel := context.WithCancel(t.Context())
	defer cancel()
	b := &bridge{root: root}
	stop := b.sampleDiagnostics(time.Now(), path)
	defer stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		r := b.diagnosticReport()
		if r.Host.CPU != nil && r.Bridge.CPU != nil {
			if r.Host.Total == nil || r.Host.Used == nil || r.Bridge.RSS == nil || *r.Bridge.RSS == 0 ||
				r.Bridge.Goroutine == 0 || r.Bridge.DB != 18 {
				t.Fatalf("incomplete sample: %+v", r.diagSystem)
			}
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for Linux CPU deltas")
		case <-tick.C:
		}
	}
	cancel()
	stop()
}

func TestDiagnosticsEndpointAndBoardHUD(t *testing.T) {
	b, base := wsHarness(t, nil)
	board := testConnectWS(t, base, "unit", false)
	const id = "echoear-diag"
	board.json(t, object{"ev": "hello", "role": "device", "proto": 2, "device_id": id, "character": "Cake", "firmware": "0.8.4",
		"board": "esp-box-3", "width": 320, "height": 240, "shape": "rect", "has_battery": false, "wifi_rssi": -60, "cpu_pct": 27,
		"free_heap": 65536, "min_free_heap": 32768, "free_psram": 6291456, "uptime_s": 320, "volume": 70})
	board.until(t, isDisplay)
	b.ws.mu.Lock()
	device := b.ws.devices[id]
	probe := device.client.ping(time.Now().Add(-12 * time.Millisecond))
	b.ws.mu.Unlock()
	board.write(t, maskedFrame(wsPong, true, []byte("not mine")))
	board.barrier(t)
	if device.client.pingRTT() != nil {
		t.Fatal("unsolicited pong was accepted")
	}
	board.write(t, maskedFrame(wsPong, true, probe.data))
	board.barrier(t)
	testRequest(t, "GET", base+"/api/diagnostics", "", nil, 401)
	testRequest(t, "GET", base+"/api/diagnostics?token=unit", "", nil, 401)
	raw := testRequest(t, "GET", base+"/api/diagnostics", "unit", nil, 200)
	var report diagReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if report.Generated.IsZero() || len(report.Devices) != 1 || report.Turns == nil || report.Latency["render_ms"].Count == 0 {
		t.Fatalf("invalid diagnostics response: %s", raw)
	}
	d := report.Devices[0]
	if !d.WS || d.Board != "esp-box-3" || d.Firmware != "0.8.4" || d.RTT == nil || *d.RTT < 12 ||
		d.CPU == nil || *d.CPU != 27 || *d.WiFi != -60 || *d.FreeHeap != 65536 || *d.MinHeap != 32768 || *d.FreePSRAM != 6291456 ||
		*d.HasBattery || d.DeviceVol == nil || *d.DeviceVol != 70 || d.Frame.Bytes >= d.Frame.RawBytes || d.Frame.Codec != "RLE565" {
		t.Fatalf("invalid device telemetry: %+v", d)
	}
	if d.LastFrameRev != nil || d.AudioDrops != nil {
		t.Fatal("older devices must not claim to report hardware counters")
	}
	board.json(t, object{"ev": "status", "last_frame_rev": 65535, "display_flush_ms": 12, "input_drops": 0, "audio_underruns": 2, "audio_drops": 3})
	board.json(t, object{"ev": "status", "cpu_pct": 27})
	board.barrier(t)
	raw = testRequest(t, "GET", base+"/api/diagnostics", "unit", nil, 200)
	report = diagReport{}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	d = report.Devices[0]
	if d.LastFrameRev == nil || *d.LastFrameRev != 65535 || d.DisplayFlushMS == nil || *d.DisplayFlushMS != 12 ||
		d.InputDrops == nil || *d.InputDrops != 0 || d.AudioUnderruns == nil || *d.AudioUnderruns != 2 ||
		d.AudioDrops == nil || *d.AudioDrops != 3 {
		t.Fatalf("invalid IO telemetry: %+v", d)
	}
	view := testConnectWS(t, base, "unit", true)
	view.json(t, object{"ev": "hello", "role": "web"})
	view.until(t, hasWS("peers", id))
	view.json(t, object{"ev": "watch", "device_id": id})
	view.until(t, hasWS("svg", "cat-ring"))
	tap := func(x, y int) { board.json(t, object{"ev": "tap", "x": x, "y": y}) }
	tap(160, 210) // Menu after the 360-square view is letterboxed into 320×240.
	view.until(t, hasWS("svg", "menu-next"))
	tap(206, 197)
	view.until(t, hasWS("svg", "menu-stats"))
	tap(116, 146)
	hud := view.until(t, hasWS("svg", "CPU 27%"))
	if !strings.Contains(string(hud.data), "Cake · 0.8.4 · esp-box-3") || !strings.Contains(string(hud.data), "USB") {
		t.Fatal("missing identity/power in HUD")
	}
	board.json(t, object{"ev": "status", "cpu_pct": 39, "wifi_rssi": -54})
	view.until(t, hasWS("svg", "CPU 39%"))
	tap(160, 210)
	view.until(t, hasWS("svg", "menu-stats"))
	tap(160, 197)
	view.until(t, hasWS("svg", "cat-ring"))
}

func TestDiagnosticsFirstSpokenDeltaAndAudio(t *testing.T) {
	b := &bridge{}
	ctx := b.traceTurn(t.Context(), "box", "Mint", "voice", time.Now().Add(-10*time.Millisecond))
	tr := traceFrom(ctx)
	text := streamedText{onFirst: func() { tr.measure(diagLLMFirst, time.Millisecond) }}
	if next := text.add(" \nHey"); len(next) != 0 || tr.record.First == nil {
		t.Fatal("time-to-first-text waited for a complete sentence")
	}
	audio := newAudioStream()
	start := time.Now()
	audio.append([]byte{1, 0})
	audio.traceFirst(tr, start)
	tr.audioEnqueued(time.Now())
	row := b.diagnosticReport().Turns[0]
	if row.TTS == nil || row.E2E == nil || *row.E2E < 10 || row.FirstPCM == nil || *row.FirstPCM != *row.E2E {
		t.Fatal(row)
	}
}

func TestDiagnosticsTTSQueueDropsCanceledSpeechWithoutRestart(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script, calls := filepath.Join(dir, "worker.py"), filepath.Join(dir, "requests")
	if err := os.WriteFile(script, []byte(`import struct, sys
r, w = sys.stdin.buffer, sys.stdout.buffer
while header := r.read(4):
    r.read(struct.unpack("<I", header)[0])
    text = r.read(struct.unpack("<I", r.read(4))[0])
    with open(sys.argv[1], "ab") as calls:
        calls.write(text + b"\n")
    pcm = struct.pack("<f", 0.25) if text else b""
    w.write(struct.pack("<BII", 0, 16000 if text else 0, len(pcm)) + pcm)
    w.flush()
`), 0600); err != nil {
		t.Fatal(err)
	}
	root, stop := context.WithCancel(t.Context())
	worker := newSynthWorker(root, python, script, calls, "")
	defer func() { stop(); <-worker.done }()
	b := &bridge{}
	ctx := b.traceTurn(root, "vocat", "Cake", "typed", time.Now())
	if pcm, err := worker.synthesizeQueued(ctx, heartVoice, "First", time.Now().Add(-10*time.Millisecond)); err != nil || len(pcm) != 2 {
		t.Fatalf("worker did not return PCM: %v, %v", pcm, err)
	}
	pid := worker.pid()
	stale, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := worker.synthesize(stale, heartVoice, "Discard me"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := worker.synthesize(ctx, heartVoice, "Second"); err != nil || worker.pid() != pid {
		t.Fatalf("worker did not stay warm: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil || string(data) != "\nFirst\nSecond\n" {
		t.Fatalf("canceled work reached Python: %q (%v)", data, err)
	}
	report := b.diagnosticReport()
	if row := report.Turns[0]; row.TTSQueue == nil || *row.TTSQueue < 10 || row.TTSInfer == nil ||
		report.Latency["tts_queue_ms"].Count != 1 || report.Latency["tts_infer_ms"].Count != 1 {
		t.Fatalf("missing split timings: %+v", row)
	}
}

func TestAccessibleRecordingControls(t *testing.T) {
	transcribed := make(chan struct{}, 2)
	_, base := wsHarness(t, func(b *bridge, dir string) {
		b.hex = setupTestHex(t, dir, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			transcribed <- struct{}{}
			_, _ = io.WriteString(w, `{"transcript":""}`)
		})
	})
	dev := testConnectWS(t, base, "unit", false)
	const id = "echoear-mic-controls"
	dev.hello(t, id, "Cake")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, id)
	tap := func(x, y int) { dev.json(t, object{"ev": "tap", "x": x, "y": y}) }
	record := func() {
		tap(180, 180)
		dev.until(t, hasWS("rec", `"action":"start"`))
		mirror.until(t, hasWS("svg", "rec-send"))
		dev.json(t, object{"ev": "rec_start"})
		dev.write(t, maskedFrame(wsBinary, true, []byte{micPrefix, 12, 0}))
	}
	record()
	tap(110, 310)
	dev.until(t, hasWS("rec", `"cancelled":true`))
	dev.until(t, hasWS("chime", `"kind":"cancel"`))
	dev.json(t, object{"ev": "rec_end"})
	dev.barrier(t)
	mirror.until(t, hasWS("svg", `data-id=\"menu\"`))
	select {
	case <-transcribed:
		t.Fatal("cancelled PCM reached Hex")
	default:
	}
	record()
	tap(250, 310)
	dev.until(t, hasWS("rec", `"action":"stop"`))
	select {
	case <-transcribed:
		// The target did not send rec_end; the fail-safe used buffered hardware PCM.
	case <-time.After(2 * time.Second):
		t.Fatal("Send did not finalize after the hardware rec_end timeout")
	}
	dev.json(t, object{"ev": "rec_end"})
	dev.barrier(t)
	raw := apiObjects(t, base+"/api/diagnostics", "recent_turns")
	if len(raw) != 1 || raw[0].(object)["source"] != "voice" {
		t.Fatal("recording did not produce exactly one voice turn", raw)
	}
}

func TestAccessibleStopAndFaceInterrupt(t *testing.T) {
	b, base := wsHarness(t, nil)
	const id = "echoear-stop"
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Cake")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, id)
	for _, target := range []string{"stop", "cat-ring"} {
		cancelled := make(chan struct{})
		b.ws.mu.Lock()
		b.ws.startTurn(b.ws.devices[id], time.Now(), false, "typed", func(ctx context.Context, _ uint64) {
			<-ctx.Done()
			close(cancelled)
		})
		b.ws.mu.Unlock()
		mirror.until(t, hasWS("svg", `data-id=\"stop\"`))
		mirror.json(t, object{"ev": "tap", "id": target})
		mirror.until(t, hasWS("svg", `data-id=\"menu\"`))
		select {
		case <-cancelled:
		case <-time.After(2 * time.Second):
			t.Fatal("did not interrupt thinking with", target)
		}
	}
}
