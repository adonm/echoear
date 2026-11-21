package main

import (
	"context"
	"strings"
	"sync/atomic"
	"time"
)

type endpointSettings struct {
	newVAD  func() endpointVAD
	streams chan struct{}
}

type endpointCapture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	packets   chan micPacket
	result    chan captureResult
	listening chan struct{}
	deadline  *time.Timer // Hub owns and stops the wall-clock watchdog.
}

type micPacket struct {
	pcm []byte
	at  time.Time
}

type captureResult struct {
	pcm            []byte
	text           string
	err            error
	stt, wallDelay time.Duration
	hasEnd         bool
}

func (h *wsHub) startEndpoint(d *wsDevice) {
	ctx, cancel := context.WithCancel(d.client.ctx)
	c := &endpointCapture{ctx: ctx, cancel: cancel, packets: make(chan micPacket, 64), result: make(chan captureResult, 1), listening: make(chan struct{})}
	d.recording.endpoint = c
	c.deadline = time.AfterFunc(endpointLimit, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.active(d) && d.recording.endpoint == c {
			h.recordingControl(d, time.Now(), false)
		}
	})
	go h.captureEndpoint(d, c)
}

func (h *wsHub) captureEndpoint(d *wsDevice, c *endpointCapture) {
	defer close(c.result)
	vad := h.b.endpoint.newVAD()
	if vad != nil {
		defer vad.close()
	}
	var s *captureHex
	streamTried := false
	var u utterance
	var end time.Time
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
			return
		case packet, more := <-c.packets:
			if !more {
				if s != nil {
					close(s.audio)
				}
				result := captureResult{pcm: u.clip(), hasEnd: !end.IsZero() && !u.policy.inVoice}
				if len(result.pcm) != 0 {
					start := time.Now()
					var ok bool
					result.text, ok = s.final(c.ctx)
					if !ok && c.ctx.Err() == nil {
						result.text, result.err = h.b.decodeClip(c.ctx, result.pcm)
					}
					result.stt = time.Since(start)
					if result.hasEnd {
						result.wallDelay = time.Since(end)
					}
				}
				c.result <- result
				return
			}
			if !streamTried {
				s = h.startCaptureHex(c.ctx)
				streamTried = true
			}
			var hint string
			if s != nil && s.ctx.Err() == nil {
				s.push(packet.pcm)
				if p := s.hint.Load(); p != nil {
					hint = *p
				}
			}
			decision := u.add(packet.pcm, vad, hint)
			if u.policy.voiced {
				at := time.Duration(len(u.pcm)/2) * time.Second / sampleRate
				end = packet.at.Add(u.policy.last - at)
			}
			if decision != endpointWait {
				h.mu.Lock()
				// Don't stop on an old decision while more speech is queued.
				if h.active(d) && d.recording.endpoint == c && d.recording.open() && len(c.packets) == 0 {
					h.recordingControl(d, time.Now(), decision == endpointDiscard)
				}
				h.mu.Unlock()
			}
		}
	}
}

func (h *wsHub) finishEndpoint(d *wsDevice, now time.Time) {
	c := d.recording.endpoint
	d.recording.endpoint = nil
	c.deadline.Stop()
	close(c.packets)
	close(c.listening)
	d.recording.clear()
	d.queuedVoice = c
	if !h.maybeStartVoice(d, now) {
		h.render(d, now, false)
	}
}

// Only one captured follow-up may wait for the current reply. It enters the
// same regular prompt gate as any other turn, without interrupt or retry.
func (h *wsHub) maybeStartVoice(d *wsDevice, now time.Time) bool {
	c := d.queuedVoice
	if c == nil || d.busy || d.stopAudio != nil || d.awaitAck || d.callState != callNone || !h.active(d) || h.parentRefresh(d.key, now) != "" {
		return false
	}
	d.queuedVoice = nil
	h.startTurn(d, now, false, "voice", func(ctx context.Context, id uint64) {
		// The capture owns Hex while the earlier PCM is drained; explicit Stop
		// also cancels that stream, without waiting for a backend response.
		stop := context.AfterFunc(ctx, c.cancel)
		defer stop()
		defer c.cancel()
		var result captureResult
		select {
		case result = <-c.result:
		case <-ctx.Done():
			return
		}
		if len(result.pcm) == 0 {
			h.finishTurn(d, id, nil, nil)
			return
		}
		if rms, ok := ambientRMS(result.pcm); ok {
			h.mu.Lock()
			if h.active(d) && d.turn == id {
				d.updateAmbient(rms)
			}
			h.mu.Unlock()
		}
		traceFrom(ctx).measure(diagSTT, result.stt)
		if result.err != nil {
			h.turnError(d, id, "Could not hear that. Try again.")
			return
		}
		if result.hasEnd {
			traceFrom(ctx).measure(diagEndpoint, result.wallDelay)
		}
		if result.text == "" {
			h.finishTurn(d, id, nil, nil)
			return
		}
		h.runTurn(ctx, d, id, result.text, "voice", result.pcm)
	})
	return true
}

type captureHex struct {
	ctx    context.Context
	cancel context.CancelFunc
	audio  chan []byte
	done   chan string
	hint   atomic.Pointer[string]
}

func (h *wsHub) startCaptureHex(ctx context.Context) *captureHex {
	slot := h.b.endpoint.streams
	if h.b.hex.config == nil {
		return nil
	}
	select {
	case slot <- struct{}{}:
	default:
		return nil // Hex permits one stream app-wide.
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &captureHex{ctx: ctx, cancel: cancel, audio: make(chan []byte, 24), done: make(chan string, 1)}
	go func() {
		defer func() { <-slot }()
		defer close(s.done)
		defer cancel()
		s.run(h.b.hex)
	}()
	return s
}

func (s *captureHex) push(pcm []byte) {
	if s.ctx.Err() != nil {
		return
	}
	select {
	case s.audio <- pcm:
	default:
		// Never backpressure voice detection or finalize a stream with a hole.
		s.cancel()
	}
}

func (s *captureHex) final(ctx context.Context) (string, bool) {
	if s == nil {
		return "", false
	}
	select {
	case text, ok := <-s.done:
		return text, ok && text != ""
	case <-ctx.Done():
		return "", false
	}
}

func (s *captureHex) run(h hexClient) {
	open, stop := context.WithTimeout(s.ctx, 750*time.Millisecond)
	stream, err := h.openStream(open)
	stop()
	if err != nil {
		return
	}
	finished := false
	defer func() {
		if !finished {
			stream.cancel()
		}
	}()
	if !stream.partials {
		return // Same whole-clip transcription used for legacy captures.
	}
	eventsCtx, stopEvents := context.WithCancel(s.ctx)
	defer stopEvents()
	go func() {
		var segments []string
		err := stream.events(eventsCtx, func(e hexStreamEvent) {
			switch e.Type {
			case "segment":
				segments = append(segments, e.Text)
				s.hint.Store(new(strings.Join(segments, " ")))
			case "partial":
				segment := strings.TrimSpace(e.Stable + " " + e.Tentative)
				if segment != "" {
					s.hint.Store(new(strings.TrimSpace(strings.Join(segments, " ") + " " + segment)))
				}
			}
		})
		if err != nil {
			s.hint.Store(nil) // Final HTTP response is still usable.
		}
	}()
	interval := min(160*time.Millisecond, stream.lease/4)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	const batch = sampleRate * 2 * 160 / 1000
	pending := make([]byte, 0, batch)
	last := time.Now()
	post := func(action string) bool {
		req, done := context.WithTimeout(s.ctx, 750*time.Millisecond)
		defer done()
		pcm := pending
		if action != "audio" {
			pcm = nil
		}
		ok := stream.post(req, action, pcm) == nil
		pending = pending[:0]
		last = time.Now()
		return ok
	}
	for s.ctx.Err() == nil {
		select {
		case <-s.ctx.Done():
			return
		case pcm, more := <-s.audio:
			if !more {
				if len(pending) > 0 && !post("audio") {
					return
				}
				req, done := context.WithTimeout(s.ctx, 3*time.Second)
				text, err := stream.finish(req)
				done()
				if err == nil {
					finished = true
					s.done <- text
				}
				return
			}
			pending = append(pending, pcm...)
			if len(pending) >= batch && !post("audio") {
				return
			}
		case now := <-tick.C:
			if len(pending) > 0 {
				if !post("audio") {
					return
				}
			} else if now.Sub(last) >= stream.lease/3 && !post("heartbeat") {
				return
			}
		}
	}
}
