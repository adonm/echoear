package main

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (h *wsHub) handle(c *wsClient, ev wsEvent) {
	h.mu.Lock()
	if h.closed || c.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	if c.scope != nil && ev.DeviceID != "" && ev.DeviceID != c.scope.ID {
		h.mu.Unlock()
		c.enqueue(textFrame(object{"op": "error", "status": 403, "error": "Device out of scope"}))
		return
	}
	now := time.Now()
	if c.role == "" {
		h.mu.Unlock()
		if ev.Event == "hello" {
			h.hello(c, ev, now)
		}
		return
	}
	defer h.mu.Unlock()
	d := c.device
	if d == nil {
		if ev.Event == "watch" && c.role == "web" {
			if ev.DeviceID == "" {
				c.watch = ""
				return
			}
			c.watch = strings.ToLower(ev.DeviceID)
			if d := h.devices[c.watch]; d != nil && h.active(d) {
				if d.svgWire.data != nil {
					c.enqueue(d.svgWire)
				}
				c.enqueue(textFrame(d.volumeFrame(false)))
				c.enqueue(textFrame(object{"op": "power", "brightness": d.brightness()}))
			} else {
				c.watch = ""
			}
			return
		}
		d = h.devices[c.watch]
		if ev.Event == "text" && ev.DeviceID != "" {
			d = h.devices[strings.ToLower(ev.DeviceID)]
		}
		if ev.Event != "tap" && ev.Event != "drag" && ev.Event != "up" && ev.Event != "text" && ev.Event != "head" {
			return
		}
	}
	if d == nil || !h.active(d) {
		return
	}
	if c.scope != nil && ev.Event == "watch" && ev.DeviceID == c.scope.ID {
		if d.svgWire.data != nil {
			c.enqueue(d.svgWire)
		}
		return
	}
	native := c.role == "device"
	switch ev.Event {
	case "status":
		d.status(ev.wsStatus)
		if d.drawer != "stats" {
			h.render(d, now, false)
		}
	case "tap":
		h.pointer(d, ev, now, native)
	case "drag", "up":
		if (native || c.role == "web") && d.displayApp != nil && d.app == d.displayApp && !d.asleep {
			h.pointer(d, ev, now, native)
		}
	case "head":
		switch ev.Action {
		case "pet":
			h.wake(d, now)
			d.pet = now.Add(300 * time.Millisecond)
			h.push(d, object{"op": "chime", "kind": "tap"}, false)
			h.render(d, now, false)
		}
	case "rec_start":
		if target, ok := recordTarget(ev.Target); ok && !d.recOpen && !d.recIgnoreStart {
			if target == "" {
				target = d.recTarget
			}
			if !d.recAt.IsZero() {
				d.recOpen = true
			} else {
				h.startRecording(d, target, now, false)
			}
		}
	case "rec_end":
		d.recIgnoreStart = false
		if !d.recAt.IsZero() {
			h.endRecording(d, ev, now)
		}
	case "text":
		text := strings.TrimSpace(ev.Text)
		if text != "" && len(text) <= 4096 {
			h.startTurn(d, now, false, func(ctx context.Context, id uint64) {
				h.runTurn(ctx, d, id, text, "typed", nil)
			})
		}
	case "intercom":
		target, ok := recordTarget(ev.Target)
		text := strings.TrimSpace(ev.Text)
		if ok && target != "" && text != "" && len(text) <= 4096 {
			h.startTurn(d, now, false, func(ctx context.Context, id uint64) {
				others, err := h.peerSnapshot(d.who.DeviceID)
				if err != nil || ctx.Err() != nil {
					h.turnError(d, id, "Could not find nearby cats.")
					return
				}
				h.sendIntercom(ctx, d, id, others, target, text, nil)
			})
		}
	case "tts_ack":
		if ev.Audio != 0 && ev.Audio != d.audio {
			return
		}
		if ev.State == "interrupted" && (d.ttsStarted || d.awaitAck) {
			h.stopPlayback(d, false)
			h.playbackDone(d, now)
		} else if ev.State == "done" && d.awaitAck {
			d.ttsStarted = false
			h.playbackDone(d, now)
		}
	}
}

func (h *wsHub) pointer(d *wsDevice, ev wsEvent, now time.Time, native bool) {
	width, height := screenSize, screenSize
	if native {
		width, height = d.screen.width, d.screen.height
	}
	if ev.X < 0 || ev.X >= width || ev.Y < 0 || ev.Y >= height || len(ev.ID) > 64 {
		return
	}
	if native {
		ev.X, ev.Y = d.screen.inverseTap(ev.X, ev.Y)
	}
	wasAsleep := d.asleep
	h.wake(d, now)
	if wasAsleep {
		h.render(d, now, false)
		return
	}
	inside := d.screen.shape != "round" || inScreenCircle(ev.X, ev.Y)
	key := ""
	if ev.ID == "" || native {
		if inside {
			key = hitID(d.svg.hits, ev.X, ev.Y)
		}
	} else if slices.ContainsFunc(d.svg.hits, func(hit hitBox) bool { return hit.id == ev.ID }) {
		key = ev.ID
	}
	if d.displayApp != nil {
		if d.app == d.displayApp {
			if i := slices.Index(d.app.devices, d); i >= 0 {
				h.updateApp(d.app, appEvent{kind: ev.Event, i: i, id: key, x: ev.X, y: ev.Y, now: now})
			}
		}
		return
	}
	a := d.scene.actions[key]
	switch a.kind {
	case "menu":
		h.chime(d, "tap")
		d.drawer, d.card, d.pending = "menu", nil, nil
		d.view = nil
		h.render(d, now, false)
	case "menu-open", "menu-back", "intercom-page":
		switch a.kind {
		case "menu-open":
			d.drawer, d.peerPage = a.value, 0
		case "menu-back":
			if d.drawer == "menu" {
				d.drawer = ""
			} else {
				d.drawer = "menu"
			}
		case "intercom-page":
			delta, _ := strconv.Atoi(a.value)
			d.peerPage += delta
		}
		h.chime(d, "tap")
		h.render(d, now, false)
	case "rec-cancel", "rec-send":
		h.recordingControl(d, now, a.kind == "rec-cancel")
	case "stop":
		h.leaveScene(d, now)
	case "cat":
		h.leaveScene(d, now)
	case "dismiss":
		d.card = nil
		h.render(d, now, false)
	case "voice-select":
		h.mu.Unlock()
		err := h.saveVoice(d.who.DeviceID, a.value)
		h.mu.Lock()
		if err == nil && h.active(d) {
			h.chime(d, "tap")
		}
	case "vol-auto", "vol-minus", "vol-plus":
		if err := h.setVolume(d.client.ctx, d, false, 0, a.kind); err != nil && h.active(d) {
			h.push(d, object{"op": "error", "status": 502, "error": err.Error()}, true)
		}
	case "listen", "reply":
		if !d.recAt.IsZero() {
			h.recordingControl(d, now, false)
		} else {
			h.startRecording(d, a.value, now, true)
		}
	}
}

func (h *wsHub) leaveScene(d *wsDevice, now time.Time) {
	if d.busy {
		h.cancelTurn(d)
	} else {
		h.cancelLocalTurn(d)
	}
	h.stopPlayback(d, true)
	h.clearView(d)
	h.render(d, now, true)
}

func (h *wsHub) clearView(d *wsDevice) {
	d.appGen++
	h.dropApp(d)
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}
