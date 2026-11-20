package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type diagMetric int

const (
	diagSTT diagMetric = iota
	diagLLMFirst
	diagLLMTotal
	diagTTSFirst
	diagE2E
	diagRender
	diagFrameBytes
	diagRawFrameBytes
	diagMetricCount
)

var diagMetricNames = [diagMetricCount]string{
	"stt_ms", "llm_first_ms", "llm_total_ms", "tts_first_ms", "e2e_ms",
	"render_ms", "frame_bytes", "frame_raw_bytes",
}

const diagInterval = 2 * time.Second

type diagRing[T any] struct {
	items []T
	next  int
}

func (r *diagRing[T]) add(value T, limit int) {
	if len(r.items) < limit {
		r.items = append(r.items, value)
	} else {
		r.items[r.next] = value
	}
	r.next = (r.next + 1) % limit
}

func (r diagRing[T]) each(visit func(T)) {
	for i := range r.items {
		visit(r.items[(r.next+i)%len(r.items)])
	}
}

func (r diagRing[T]) last() (out T) {
	if n := len(r.items); n > 0 {
		return r.items[(r.next+n-1)%n]
	}
	return out
}

type diagSummary struct {
	Last    *float64  `json:"last"`
	P50     *float64  `json:"p50"`
	P95     *float64  `json:"p95"`
	Count   int       `json:"count"`
	Samples []float64 `json:"samples"`
}

func summarize(r diagRing[float64]) diagSummary {
	s := diagSummary{Count: len(r.items), Samples: make([]float64, 0, len(r.items))}
	r.each(func(v float64) { s.Samples = append(s.Samples, v) })
	if s.Count != 0 {
		s.Last = new(r.last())
		ordered := slices.Clone(s.Samples)
		slices.Sort(ordered)
		// Nearest-rank percentiles; the window contains at most 32 samples.
		s.P50 = &ordered[(s.Count*50+99)/100-1]
		s.P95 = &ordered[(s.Count*95+99)/100-1]
	}
	return s
}

type diagTurn struct {
	TS        int64    `json:"ts_ms"`
	Device    string   `json:"device"`
	Character string   `json:"character"`
	Source    string   `json:"source"`
	STT       *float64 `json:"stt_ms"`
	First     *float64 `json:"llm_first_ms"`
	Total     *float64 `json:"llm_total_ms"`
	TTS       *float64 `json:"tts_first_ms"`
	E2E       *float64 `json:"e2e_ms"`
}

type diagFrame struct {
	MS       float64 `json:"render_ms"`
	Bytes    int     `json:"frame_bytes"`
	RawBytes int     `json:"frame_raw_bytes"`
	Codec    string  `json:"frame_codec"`
}

type diagnostics struct {
	mu      sync.Mutex
	metrics [diagMetricCount]diagRing[float64]
	turns   diagRing[*diagTurn]
	system  diagSystem
}

type turnTrace struct {
	d      *diagnostics
	record *diagTurn
	recEnd time.Time
}

type traceKey struct{}

func (b *bridge) traceTurn(ctx context.Context, device, character, source string, at time.Time) context.Context {
	t := &turnTrace{d: &b.diag, record: &diagTurn{TS: at.UnixMilli(), Device: device, Character: character, Source: source}}
	if source == "voice" {
		t.recEnd = at
	}
	b.diag.mu.Lock()
	b.diag.turns.add(t.record, 20)
	b.diag.mu.Unlock()
	return context.WithValue(ctx, traceKey{}, t)
}

func traceFrom(ctx context.Context) *turnTrace {
	t, _ := ctx.Value(traceKey{}).(*turnTrace)
	return t
}

func milliseconds(d time.Duration) float64 {
	return math.Round(max(0, float64(d)/float64(time.Millisecond))*100) / 100
}

func (t *turnTrace) measure(metric diagMetric, elapsed time.Duration) {
	if t == nil {
		return
	}
	value := milliseconds(elapsed)
	t.d.mu.Lock()
	defer t.d.mu.Unlock()
	field := [...]**float64{&t.record.STT, &t.record.First, &t.record.Total, &t.record.TTS, &t.record.E2E}[metric]
	if *field == nil {
		*field = &value
		t.d.metrics[metric].add(value, 32)
	}
}

func (t *turnTrace) audioEnqueued(at time.Time) {
	if t != nil && !t.recEnd.IsZero() {
		t.measure(diagE2E, at.Sub(t.recEnd))
	}
}

func (b *bridge) transcribe(ctx context.Context, pcm []byte, kind string) (string, error) {
	start := time.Now()
	text, err := b.hex.transcribe(ctx, pcm, kind)
	traceFrom(ctx).measure(diagSTT, time.Since(start))
	return text, err
}

func (h *wsHub) trackFrame(d *wsDevice, elapsed time.Duration, size, raw int, codec string) {
	d.frame = &diagFrame{milliseconds(elapsed), size, raw, codec}
	if codec == "svg" {
		return
	}
	stats := &h.b.diag
	stats.mu.Lock()
	stats.metrics[diagRender].add(d.frame.MS, 32)
	stats.metrics[diagFrameBytes].add(float64(size), 32)
	stats.metrics[diagRawFrameBytes].add(float64(raw), 32)
	stats.mu.Unlock()
}

type diagProcess struct {
	CPU *float64 `json:"cpu_pct"`
	RSS *uint64  `json:"rss_bytes"`
}

type diagBridge struct {
	diagProcess
	HeapAlloc uint64 `json:"heap_alloc_bytes"`
	HeapInuse uint64 `json:"heap_inuse_bytes"`
	Goroutine int    `json:"goroutines"`
	Uptime    int64  `json:"uptime_s"`
	DB        int64  `json:"db_bytes"`
}

type diagWorker struct {
	diagProcess
	PID   int  `json:"pid"`
	Ready bool `json:"ready"`
}

type diagHost struct {
	CPU   *float64 `json:"cpu_pct"`
	Total *uint64  `json:"mem_total_bytes"`
	Used  *uint64  `json:"mem_used_bytes"`
	Load  *float64 `json:"load_1m"`
}

type diagServices struct {
	Hex        bool `json:"hex"`
	OpenCode   bool `json:"opencode"`
	TTS        bool `json:"tts"`
	OpenTunnel bool `json:"opentunnel"`
}

type diagSystem struct {
	Bridge   diagBridge   `json:"bridge"`
	Worker   diagWorker   `json:"tts_worker"`
	Host     diagHost     `json:"host"`
	Services diagServices `json:"services"`
}

type diagReport struct {
	Generated time.Time              `json:"generated_at"`
	Latency   map[string]diagSummary `json:"latency"`
	Turns     []diagTurn             `json:"recent_turns"`
	Devices   []diagDevice           `json:"devices"`
	diagSystem
}

func (b *bridge) diagnosticReport() diagReport {
	out := diagReport{Generated: time.Now().UTC(), Latency: make(map[string]diagSummary), Turns: []diagTurn{}, Devices: []diagDevice{}}
	b.diag.mu.Lock()
	out.diagSystem = b.diag.system
	for i, name := range diagMetricNames {
		out.Latency[name] = summarize(b.diag.metrics[i])
	}
	b.diag.turns.each(func(t *diagTurn) { out.Turns = append(out.Turns, *t) })
	b.diag.mu.Unlock()
	slices.Reverse(out.Turns)
	if b.ws != nil {
		out.Devices = b.ws.diagDevices()
	}
	return out
}

type diagDevice struct {
	ID        string     `json:"device_id"`
	Character string     `json:"character"`
	Kind      string     `json:"kind"`
	Board     string     `json:"board,omitempty"`
	Firmware  string     `json:"firmware,omitempty"`
	OTA       string     `json:"ota,omitempty"`
	OTABundle bool       `json:"ota_bundle"`
	WS        bool       `json:"ws_connected"`
	Mode      string     `json:"mode"`
	RTT       *float64   `json:"rtt_ms"`
	Frame     *diagFrame `json:"last_frame"`
	Ambient   *float64   `json:"ambient_rms"`
	Auto      bool       `json:"auto_volume"`
	DeviceVol *int       `json:"reported_volume"`
	wsStatus
}

func (d *wsDevice) diagDevice() diagDevice {
	s := d.telemetry
	s.Volume = new(d.vol)
	out := diagDevice{ID: d.who.DeviceID, Character: d.cat.Name, Kind: "physical", Board: d.board,
		Firmware: d.firmware, OTA: s.Partition, WS: true, Mode: d.pose.mood, RTT: d.client.pingRTT(),
		Frame: d.frame, Auto: d.autoVol, DeviceVol: d.telemetry.Volume, wsStatus: s}
	if d.virtual() {
		out.Kind = "virtual"
	}
	if d.hasAmbient {
		out.Ambient = new(d.ambientEMA)
	}
	return out
}

func (h *wsHub) diagDevices() []diagDevice {
	virtual, _ := h.b.virtualSnapshot()
	roundOTA, boxOTA := fileBytes(h.b.ota.path) > 0, fileBytes(h.b.box3OTA.path) > 0
	h.mu.Lock()
	defer h.mu.Unlock()
	peers, _ := h.combinedPeers("")
	devices := make(map[string]diagDevice, len(peers)+len(virtual))
	for _, p := range peers {
		id := strings.ToLower(p.DeviceID)
		devices[id] = diagDevice{ID: p.DeviceID, Character: p.Character, Kind: "physical", Mode: "offline"}
	}
	for id, v := range virtual {
		devices[id] = diagDevice{ID: id, Character: h.b.catFor(peer{DeviceID: id, Character: v.Character}).Name, Kind: "virtual", Mode: "offline"}
	}
	for id, d := range h.devices {
		if h.active(d) {
			device := d.diagDevice()
			device.OTABundle = device.Board == "esp-box-3" && boxOTA || device.Board == "esp-vocat" && roundOTA
			devices[id] = device
		}
	}
	out := make([]diagDevice, 0, len(devices))
	for _, d := range devices {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b diagDevice) int { return strings.Compare(a.ID, b.ID) })
	return out
}

type cpuReading struct{ user, total uint64 }
type procReading struct {
	pid     int
	ticks   uint64
	started uint64
}

func fileText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func fileBytes(path string) int64 {
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		return info.Size()
	}
	return 0
}

func linuxClockTicks() float64 {
	vec, _ := os.ReadFile("/proc/self/auxv")
	word := strconv.IntSize / 8
	for i := 0; i+2*word <= len(vec); i += 2 * word {
		read := func(b []byte) uint64 {
			if word == 4 {
				return uint64(binary.NativeEndian.Uint32(b))
			}
			return binary.NativeEndian.Uint64(b)
		}
		if read(vec[i:]) == 17 { // AT_CLKTCK
			if n := read(vec[i+word:]); n != 0 {
				return float64(n)
			}
		}
	}
	return 100 // Linux USER_HZ on supported architectures.
}

func parseProcStat(raw string, pid int) procReading {
	// comm is parenthesized and may contain spaces or closing parentheses.
	i := strings.LastIndexByte(raw, ')')
	if i < 0 {
		return procReading{}
	}
	fields := strings.Fields(raw[i+1:])
	if len(fields) < 20 {
		return procReading{}
	}
	user, e1 := strconv.ParseUint(fields[11], 10, 64)
	system, e2 := strconv.ParseUint(fields[12], 10, 64)
	started, e3 := strconv.ParseUint(fields[19], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return procReading{}
	}
	return procReading{pid, user + system, started}
}

func sampleProcess(path string, pid int, prev *procReading, seconds, hz float64) diagProcess {
	var out diagProcess
	next := parseProcStat(fileText(path+"/stat"), pid)
	if next.pid == prev.pid && next.pid != 0 && next.started == prev.started && next.ticks >= prev.ticks && seconds > 0 {
		out.CPU = new(100 * float64(next.ticks-prev.ticks) / (hz * seconds))
	}
	*prev = next
	if fields := strings.Fields(fileText(path + "/statm")); len(fields) > 1 {
		if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
			out.RSS = new(pages * uint64(os.Getpagesize()))
		}
	}
	return out
}

func parseHostCPU(raw string) (out cpuReading) {
	fields := strings.Fields(strings.SplitN(raw, "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return out
	}
	for i, s := range fields[1:min(len(fields), 9)] {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuReading{}
		}
		out.total += n
		if i != 3 && i != 4 { // idle and iowait; guest time is already in user/nice.
			out.user += n
		}
	}
	return out
}

func sampleHost(prev *cpuReading) (out diagHost) {
	next := parseHostCPU(fileText("/proc/stat"))
	if prev.total != 0 && next.total > prev.total && next.user >= prev.user {
		out.CPU = new(min(100., 100*float64(next.user-prev.user)/float64(next.total-prev.total)))
	}
	*prev = next
	var available *uint64
	for line := range strings.SplitSeq(fileText("/proc/meminfo"), "\n") {
		key, value, _ := strings.Cut(line, ":")
		if key != "MemTotal" && key != "MemAvailable" {
			continue
		}
		kb, _, _ := strings.Cut(strings.TrimSpace(value), " ")
		if n, err := strconv.ParseUint(kb, 10, 64); err == nil {
			if key == "MemTotal" {
				out.Total = new(n * 1024)
			} else {
				available = new(n * 1024)
			}
		}
	}
	if out.Total != nil && available != nil && *out.Total >= *available {
		out.Used = new(*out.Total - *available)
	}
	if one, _, _ := strings.Cut(fileText("/proc/loadavg"), " "); one != "" {
		if n, err := strconv.ParseFloat(one, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
			out.Load = &n
		}
	}
	return out
}

func (b *bridge) sampleDiagnostics(start time.Time, path string) func() {
	ctx, cancel := context.WithCancel(b.root)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(diagInterval)
		defer ticker.Stop()
		hz := linuxClockTicks()
		var self, worker procReading
		var cpu cpuReading
		last := time.Now()
		for ctx.Err() == nil {
			now := time.Now()
			seconds := now.Sub(last).Seconds()
			last = now
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			report := diagSystem{Bridge: diagBridge{
				diagProcess: sampleProcess("/proc/self", os.Getpid(), &self, seconds, hz),
				HeapAlloc:   stats.HeapAlloc, HeapInuse: stats.HeapInuse, Goroutine: runtime.NumGoroutine(),
				Uptime: int64(now.Sub(start) / time.Second), DB: fileBytes(path) + fileBytes(path+"-wal"),
			}, Host: sampleHost(&cpu)}
			if b.worker != nil {
				report.Worker.PID, report.Worker.Ready = b.worker.pid(), b.worker.ready.Load()
				if report.Worker.PID > 0 {
					report.Worker.diagProcess = sampleProcess(fmt.Sprintf("/proc/%d", report.Worker.PID), report.Worker.PID, &worker, seconds, hz)
				} else {
					worker = procReading{}
				}
				report.Services.TTS = report.Worker.Ready
			}
			report.Services.OpenTunnel = b.tunnels != nil && b.tunnels.runner != nil
			var wg sync.WaitGroup
			if b.hex.config != nil {
				wg.Go(func() { report.Services.Hex = b.hex.reachable(ctx, "/health") })
			}
			if b.oc != nil && b.oc.config != nil {
				wg.Go(func() { report.Services.OpenCode = b.oc.reachable(ctx, "/api/info") })
			}
			wg.Wait()
			b.diag.mu.Lock()
			b.diag.system = report
			b.diag.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
