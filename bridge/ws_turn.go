package main

import (
	"context"
	"strings"
	"time"
)

const (
	micPrefix = 0x01
	pcmPrefix = 0x02
	pcmPacket = 16 << 10
)

func recordTarget(s string) (string, bool) {
	s = strings.TrimSpace(s)
	return s, s == "" || targetRE.MatchString(s)
}

func (h *wsHub) clearRecording(d *wsDevice) {
	d.recAt, d.recOpen, d.recTarget, d.mic = time.Time{}, false, "", nil
}

func (h *wsHub) startRecording(d *wsDevice, target string, now time.Time, request bool) {
	h.wake(d, now)
	waiting := target == "" && h.hasWaiter(d)
	if !waiting {
		h.cancelTurn(d)
		d.drawer, d.view, d.pending, d.customScene = false, nil, nil, false
	}
	h.stopPlayback(d, true)
	d.recAt, d.recOpen, d.recTarget, d.mic = now, !request, target, nil
	if request {
		h.push(d, object{"op": "rec", "action": "start", "target": target}, false)
	}
	h.render(d, now, false)
}

func (h *wsHub) micChunk(c *wsClient, data []byte) {
	if len(data) < 3 || data[0] != micPrefix || len(data)%2 != 1 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d := c.device
	if d == nil || !h.active(d) || !d.recOpen {
		return
	}
	if len(data)-1 > maxPCM-len(d.mic) {
		h.clearRecording(d)
		h.push(d, object{"op": "rec", "action": "stop", "target": ""}, false)
		h.push(d, object{"op": "chime", "kind": "cancel"}, false)
		h.render(d, time.Now(), false)
		return
	}
	d.mic = append(d.mic, data[1:]...)
}

func (h *wsHub) endRecording(d *wsDevice, ev wsEvent, now time.Time) {
	pcm, target := d.mic, d.recTarget
	if requested, ok := recordTarget(ev.Target); ok && requested != "" {
		target = requested
	}
	h.clearRecording(d)
	if ev.Cancelled || len(pcm) == 0 {
		if ev.Cancelled {
			h.push(d, object{"op": "chime", "kind": "cancel"}, false)
		}
		h.wake(d, now)
		h.render(d, now, false)
		return
	}
	if target == "" && h.hasWaiter(d) {
		h.render(d, now, false)
		go func() {
			ctx, cancel := context.WithTimeout(d.client.ctx, turnTimeout)
			defer cancel()
			if text, err := h.b.hex.transcribe(ctx, pcm, pcmType); err == nil && text != "" {
				h.b.deviceEvent(d.who.DeviceID, "speech", object{"text": text, "source": "voice"})
			}
		}()
		return
	}
	h.startTurn(d, now, false, func(ctx context.Context, id uint64) {
		h.runVoice(ctx, d, id, target, pcm)
	})
}

func (h *wsHub) cancelTurn(d *wsDevice) {
	d.turn++
	d.busy = false
	d.agentSetScene, d.agentSpoke = false, false
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
}

func (h *wsHub) startTurn(d *wsDevice, now time.Time, keepView bool, run func(context.Context, uint64)) {
	h.wake(d, now)
	h.cancelTurn(d)
	h.stopPlayback(d, true)
	if !d.recAt.IsZero() {
		h.push(d, object{"op": "rec", "action": "stop", "target": d.recTarget}, false)
		h.clearRecording(d)
	}
	d.drawer, d.pending = false, nil
	if !keepView {
		d.view, d.customScene = nil, false
	}
	ctx, cancel := context.WithTimeout(d.client.ctx, turnTimeout)
	id := d.turn
	d.cancel, d.busy = cancel, true
	if !keepView {
		h.push(d, object{"op": "chime", "kind": "think"}, false)
	}
	h.render(d, now, false)
	go run(ctx, id)
}

func (h *wsHub) finishTurn(d *wsDevice, id uint64, view *sceneView) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active(d) || d.turn != id {
		return
	}
	d.busy = false
	now := time.Now()
	h.wake(d, now)
	if view != nil && !d.agentSetScene {
		if d.stopAudio != nil || d.awaitAck {
			d.pending = view
		} else {
			d.view = view
		}
	}
	if d.stopAudio == nil && d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	h.render(d, now, false)
}

func (h *wsHub) turnError(d *wsDevice, id uint64, message string) {
	view := noticeScene("Try again", message)
	h.finishTurn(d, id, &view)
}

func (h *wsHub) resetTurn(ctx context.Context, d *wsDevice, id uint64) {
	if h.b.oc != nil {
		if err := h.b.oc.reset(ctx, d.who.DeviceID); err != nil {
			h.turnError(d, id, "Could not start a new chat.")
			return
		}
	}
	h.finishTurn(d, id, nil)
}

func (h *wsHub) runVoice(ctx context.Context, d *wsDevice, id uint64, target string, pcm []byte) {
	if target != "" {
		others, err := h.peerSnapshot(d.who.DeviceID)
		if err != nil {
			h.turnError(d, id, "Could not find nearby cats.")
			return
		}
		recipients := targets(others, target)
		receipts, legacy := h.deliverOnline(d.who, recipients, "", pcm)
		// Connected peers can play the original clip before Hex has generated a caption.
		if len(receipts) != 0 && len(legacy) == 0 {
			go func() {
				captionCtx, cancel := context.WithTimeout(h.b.root, 30*time.Second)
				defer cancel()
				transcript, _ := h.b.hex.transcribe(captionCtx, pcm, pcmType)
				h.caption(receipts, d.who, transcript)
				ids := make([]string, 0, len(receipts))
				for _, r := range receipts {
					ids = append(ids, r.device.who.DeviceID)
				}
				h.b.recordIntercom(d.who, ids, transcript)
			}()
			h.confirm(ctx, d, id, target, recipients, true, false)
			return
		}
		var transcript string
		if len(recipients) > 0 {
			transcript, _ = h.b.hex.transcribe(ctx, pcm, pcmType)
			h.caption(receipts, d.who, transcript)
		}
		res := h.b.deliverHTTP(ctx, d.who, legacy, transcript, pcm)
		for _, r := range receipts {
			res.sent = append(res.sent, r.device.who.DeviceID)
		}
		h.b.recordIntercom(d.who, res.sent, transcript)
		h.confirm(ctx, d, id, target, recipients, len(res.sent) != 0, len(res.sent) < len(recipients))
		return
	}
	text, err := h.b.hex.transcribe(ctx, pcm, pcmType)
	if err != nil {
		h.turnError(d, id, "Could not hear that. Try again.")
		return
	}
	if text == "" {
		h.finishTurn(d, id, nil)
		return
	}
	h.b.deviceEvent(d.who.DeviceID, "speech", object{"text": text, "source": "voice"})
	h.runTurn(ctx, d, id, text, pcm)
}

func (h *wsHub) runTurn(ctx context.Context, d *wsDevice, id uint64, text string, pcm []byte) {
	others, err := h.peerSnapshot(d.who.DeviceID)
	if err != nil || ctx.Err() != nil {
		h.turnError(d, id, "Could not find nearby cats.")
		return
	}
	source := "text"
	if pcm != nil {
		source = "voice"
	}
	if direct := directMessage(others, text); direct != nil {
		h.b.chat(d.who.DeviceID, newChat("user", "You", text, source), true)
		h.sendIntercom(ctx, d, id, others, direct.target, direct.text, pcm)
		return
	}
	if h.b.oc == nil {
		h.turnError(d, id, "Chat is not ready yet.")
		return
	}
	gen, err := h.b.generateReply(ctx, ctx, d.who, others, text, turnOptions{source: source,
		publish: func(stream *AudioStream) { h.publish(d, id, ctx, stream) },
		agentSpoke: func() bool {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.active(d) && d.turn == id && d.agentSpoke
		}})
	if err != nil {
		h.turnError(d, id, "Could not finish that turn.")
		return
	}
	if gen.message != nil && ctx.Err() == nil {
		h.b.deliver(ctx, d.who, targets(others, gen.message.target), gen.message.text, nil)
	}
	var view *sceneView
	if gen.screen != nil {
		v := screenScene(gen.screen, catPalette(d.who.Character).accent)
		view = &v
	} else if gen.audio == nil && gen.reply != "" && !gen.spoken {
		v := noticeScene("Answer", shortText(gen.reply, maxMessage, true))
		view = &v
	}
	h.finishTurn(d, id, view)
}

func (h *wsHub) sendIntercom(ctx context.Context, d *wsDevice, id uint64, others []peer, target, text string, pcm []byte) {
	recipients := targets(others, target)
	res := h.b.deliver(ctx, d.who, recipients, text, pcm)
	h.confirm(ctx, d, id, target, recipients, len(res.sent) != 0, len(res.failed) != 0)
}

func (h *wsHub) confirm(ctx context.Context, d *wsDevice, id uint64, target string, recipients []peer, sent, partial bool) {
	if broadcast(target) {
		target = "everyone"
	} else if len(recipients) != 0 {
		target = recipients[0].Character
	}
	reply := "Sent to " + target + "!"
	title := "Message sent"
	switch {
	case !sent:
		reply, title = "I couldn't reach "+target+" right now.", "Not delivered"
	case partial:
		reply, title = "I sent your message, but couldn't reach everyone.", "Partly delivered"
	}
	view := noticeScene(title, reply)
	h.sayTurn(ctx, d, id, reply, &view)
}

func (h *wsHub) sayTurn(ctx context.Context, d *wsDevice, id uint64, text string, view *sceneView) {
	w := h.b.worker
	if w != nil && w.ready.Load() {
		stream := newAudioStream()
		h.publish(d, id, ctx, stream)
		pcm := h.b.clips.peek(text)
		if pcm == nil {
			clean, err := spokenText(text)
			if err == nil {
				pcm, _ = w.synthesize(ctx, clean)
			}
		}
		if !stream.append(pcm) {
			stream.finish(errTTS)
		} else {
			stream.finish(nil)
		}
	}
	h.finishTurn(d, id, view)
}

func (h *wsHub) publish(d *wsDevice, id uint64, ctx context.Context, stream *AudioStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active(d) || d.turn != id || ctx.Err() != nil {
		return
	}
	if stream == nil {
		h.stopPlayback(d, true)
		h.render(d, time.Now(), false)
		return
	}
	h.play(d, ctx, stream)
}

func (h *wsHub) play(d *wsDevice, ctx context.Context, stream *AudioStream) {
	h.stopPlayback(d, true)
	ctx, cancel := context.WithCancel(ctx)
	d.stopAudio = cancel
	id := d.audio
	go func() {
		defer cancel()
		failed := false
		for i := 0; ; i++ {
			pcm, err := stream.chunk(ctx, i)
			if len(pcm) == 0 || err != nil || ctx.Err() != nil {
				failed = err != nil || ctx.Err() != nil
				break
			}
			frames := make([]wsFrame, 0, (len(pcm)+pcmPacket-1)/pcmPacket)
			for offset := 0; offset < len(pcm); offset += pcmPacket {
				frames = append(frames, wsFrame{wsBinary,
					append(append(make([]byte, 0, 1+min(pcmPacket, len(pcm)-offset)), pcmPrefix),
						pcm[offset:min(offset+pcmPacket, len(pcm))]...)})
			}
			h.mu.Lock()
			if !h.active(d) || d.audio != id || ctx.Err() != nil {
				h.mu.Unlock()
				failed = true
				break
			}
			now := time.Now()
			if !d.ttsStarted {
				d.ttsStarted = true
				d.audioUntil = now
				h.push(d, object{"op": "tts", "state": "start", "audio": id}, false)
				h.render(d, now, false)
			}
			if d.audioUntil.Before(now) {
				d.audioUntil = now
			}
			d.audioUntil = d.audioUntil.Add(time.Duration(len(pcm)) * time.Second / (2 * sampleRate))
			for _, f := range frames {
				if !d.client.enqueue(f) {
					break
				}
			}
			h.mu.Unlock()
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.active(d) || d.audio != id {
			return
		}
		d.stopAudio = nil
		now := time.Now()
		if failed || !d.ttsStarted {
			h.stopPlayback(d, failed)
			h.playbackDone(d, now)
		} else {
			h.push(d, object{"op": "tts", "state": "end", "audio": id}, false)
			d.ttsStarted, d.awaitAck = false, true
		}
		if !d.busy && d.cancel != nil {
			d.cancel()
			d.cancel = nil
		}
	}()
}

func (h *wsHub) stopPlayback(d *wsDevice, notify bool) {
	if d.stopAudio != nil {
		d.stopAudio()
		d.stopAudio = nil
	}
	if d.ttsStarted {
		h.push(d, object{"op": "tts", "state": "end", "audio": d.audio}, false)
	}
	if notify && (d.ttsStarted || d.awaitAck) {
		h.push(d, object{"op": "chime", "kind": "cancel"}, false)
	}
	d.ttsStarted, d.awaitAck = false, false
	d.audio++
}

func (h *wsHub) playbackDone(d *wsDevice, now time.Time) {
	d.awaitAck = false
	if d.pending != nil {
		d.view, d.pending = d.pending, nil
	}
	h.wake(d, now)
	h.render(d, now, false)
}

type wsReceipt struct {
	device *wsDevice
	view   *sceneView
}

func (h *wsHub) deliverOnline(sender peer, recipients []peer, text string, pcm []byte) ([]wsReceipt, []peer) {
	h.mu.Lock()
	var connected []*wsDevice
	legacy := make([]peer, 0, len(recipients))
	for _, p := range recipients {
		if d := h.devices[strings.ToLower(p.DeviceID)]; d != nil && h.active(d) {
			connected = append(connected, d)
		} else {
			legacy = append(legacy, p)
		}
	}
	h.mu.Unlock()
	if len(connected) == 0 {
		return nil, recipients
	}
	text = shortText(text, maxMessage, true)
	if len(pcm) > maxPeerPCM {
		return nil, legacy
	}
	stream := newAudioStream()
	if pcm != nil {
		intro := h.b.clips.peek("From " + sender.Character + "!")
		if len(intro)+len(pcm) <= maxPeerPCM && intro != nil {
			stream.append(intro)
		}
		stream.append(NormalizeVoicePCM(pcm))
		stream.finish(nil)
	}
	receipts := make([]wsReceipt, 0, len(connected))
	h.mu.Lock()
	for _, d := range connected {
		if !h.active(d) {
			continue
		}
		view := intercomScene(sender, text)
		if !h.hasWaiter(d) {
			h.cancelTurn(d)
			h.stopPlayback(d, true)
			if !d.recAt.IsZero() {
				h.push(d, object{"op": "rec", "action": "stop", "target": d.recTarget}, false)
				h.clearRecording(d)
			}
			d.view, d.drawer, d.pending, d.customScene = &view, false, nil, false
		}
		now := time.Now()
		h.wake(d, now)
		h.render(d, now, false)
		h.push(d, object{"op": "chime", "kind": "intercom"}, false)
		if d.client.ctx.Err() == nil {
			receipts = append(receipts, wsReceipt{d, &view})
			h.play(d, d.client.ctx, stream)
		}
	}
	h.mu.Unlock()
	if pcm == nil && len(receipts) > 0 {
		go h.speakIntercom(stream, sender, text)
	}
	return receipts, legacy
}

func (h *wsHub) speakIntercom(stream *AudioStream, sender peer, text string) {
	w := h.b.worker
	if w == nil || !w.ready.Load() {
		stream.finish(nil) // The card is still delivered when TTS is unavailable.
		return
	}
	ctx, cancel := context.WithTimeout(h.b.root, ttsWait)
	defer cancel()
	intro, _ := h.b.clips.get(ctx, w, "Message from "+sender.Character+"!")
	if len(intro) > 0 {
		stream.append(intro)
	}
	for _, s := range SplitSentences(text) {
		pcm, err := w.synthesize(ctx, s)
		if err != nil || !stream.append(pcm) {
			stream.finish(errTTS)
			return
		}
	}
	stream.finish(nil)
}

func (h *wsHub) caption(receipts []wsReceipt, sender peer, text string) {
	if len(receipts) == 0 || text == "" {
		return
	}
	view := intercomScene(sender, shortText(text, maxMessage, true))
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range receipts {
		if h.active(r.device) && r.device.view == r.view {
			r.device.view = &view
			h.render(r.device, time.Now(), false)
		}
	}
}

func (b *bridge) deliver(ctx context.Context, sender peer, recipients []peer, text string, raw []byte) (result delivery) {
	defer func() { b.recordIntercom(sender, result.sent, text) }()
	if b.ws == nil || len(recipients) == 0 {
		return b.deliverHTTP(ctx, sender, recipients, text, raw)
	}
	ws, rest := b.ws.deliverOnline(sender, recipients, text, raw)
	if len(rest) == len(recipients) {
		return b.deliverHTTP(ctx, sender, recipients, text, raw)
	}
	sent := make(map[string]bool, len(recipients))
	for _, r := range ws {
		sent[r.device.who.DeviceID] = true
	}
	out := delivery{sent: []string{}, delivered: []string{}, failed: []string{}}
	if len(rest) != 0 {
		fallback := b.deliverHTTP(ctx, sender, rest, text, raw)
		for _, id := range fallback.sent {
			sent[id] = true
		}
		out.err = fallback.err
	}
	for _, p := range recipients {
		if sent[p.DeviceID] {
			out.sent = append(out.sent, p.DeviceID)
			out.delivered = append(out.delivered, p.Character)
		} else {
			out.failed = append(out.failed, p.DeviceID)
		}
	}
	if len(out.sent) > 0 {
		out.err = ""
	} else if raw != nil && len(raw) > maxPeerPCM {
		out.err = "message_too_long"
	} else if out.err == "" {
		out.err = "delivery_failed"
	}
	return out
}

func (b *bridge) recordIntercom(sender peer, ids []string, text string) {
	if len(ids) == 0 {
		return
	}
	text = shortText(text, maxMessage, true)
	if text == "" {
		text = "Voice message"
	}
	b.chat(sender.DeviceID, newChat("intercom", sender.Character, text, "intercom"), true)
	for _, id := range ids {
		b.deviceEvent(id, "intercom", object{"from_device_id": sender.DeviceID, "sender": sender.Character, "text": text})
		b.chat(id, newChat("intercom", sender.Character, text, "intercom"), true)
	}
}
