package main

import (
	"context"
	"time"
)

type endpointSettings struct {
	newVAD func() endpointVAD
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
	var u utterance
	var end time.Time
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
			return
		case packet, more := <-c.packets:
			if !more {
				result := captureResult{pcm: u.clip(), hasEnd: !end.IsZero() && !u.policy.inVoice}
				if len(result.pcm) != 0 && c.ctx.Err() == nil {
					start := time.Now()
					result.text, result.err = h.b.decodeClip(c.ctx, result.pcm)
					result.stt = time.Since(start)
					if result.hasEnd {
						result.wallDelay = time.Since(end)
					}
				}
				c.result <- result
				return
			}
			decision := u.add(packet.pcm, vad, "")
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
		// Explicit Stop also cancels any outstanding work for this capture.
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
