package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type callState uint8

const (
	callNone callState = iota
	callRingingOut
	callRingingIn
	callActive
)

const (
	callRingEvery = 2500 * time.Millisecond
	callRingLimit = 25 * time.Second
	callPCMBytes  = 2 * sampleRate * 40 / 1000
)

// Call state and control/PCM ordering are owned by the hub lock.
func (h *wsHub) callPeer(d *wsDevice) *wsDevice {
	p := h.devices[d.callPeer]
	if d.callID == 0 || p == nil || p.callID != d.callID || p.callPeer != d.key {
		return nil
	}
	return p
}

func (h *wsHub) canCall(d *wsDevice, peerID string) bool {
	p := h.devices[strings.ToLower(peerID)]
	return d.caps.Call && p != nil && p != d && h.active(p) && p.caps.Call
}

func (h *wsHub) interruptForCall(d *wsDevice, now time.Time) {
	if d.busy {
		h.cancelTurn(d)
	} else {
		h.cancelLocalTurn(d)
	}
	h.stopPlayback(d, true)
	if !d.recAt.IsZero() {
		h.push(d, object{"op": "rec", "action": "stop", "target": d.recTarget, "cancelled": true}, false)
		h.clearRecording(d)
	}
	h.clearView(d)
	h.wake(d, now)
}

func (h *wsHub) startCall(caller *wsDevice, peerID string) {
	h.ringCall(caller, peerID, callRingLimit)
}

func (h *wsHub) ringCall(caller *wsDevice, peerID string, limit time.Duration) {
	if !h.active(caller) || caller.callState != callNone {
		return
	}
	now := time.Now()
	key := strings.ToLower(peerID)
	callee := h.devices[key]
	name := peerID
	if callee != nil {
		name = callee.cat.Name
	} else {
		for _, p := range h.storedPeers {
			if strings.EqualFold(p.DeviceID, key) {
				name = p.Character
				break
			}
		}
	}
	notice := ""
	switch {
	case !caller.caps.Call:
		notice = "This cat needs call support (firmware 0.9.2 or newer)."
	case callee == caller:
		notice = "Choose another cat to call."
	case callee == nil || !h.active(callee):
		notice = name + " is offline."
	case !callee.caps.Call:
		notice = name + " needs firmware 0.9.2 or newer to call."
	case callee.callState != callNone:
		notice = name + " is busy."
	}
	if notice != "" {
		h.interruptForCall(caller, now)
		view := noticeScene("Can't call", notice)
		caller.view = &view
		h.render(caller, now, false)
		return
	}
	h.nextCall++
	if h.nextCall == 0 {
		h.nextCall++
	}
	id := h.nextCall
	ctx, cancel := context.WithTimeout(caller.client.ctx, limit)
	h.interruptForCall(caller, now)
	h.interruptForCall(callee, now)
	caller.callState, callee.callState = callRingingOut, callRingingIn
	caller.callPeer, callee.callPeer = callee.key, caller.key
	caller.callID, callee.callID = id, id
	caller.callCancel = cancel
	for _, d := range []*wsDevice{caller, callee} {
		h.chime(d, "ring")
		// A new call may have the same artwork as the one that just ended.
		h.render(d, now, true)
	}
	go h.ringUntilAnswered(ctx, caller, id)
}

func (h *wsHub) ringUntilAnswered(ctx context.Context, caller *wsDevice, id uint32) {
	tick := time.NewTicker(callRingEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				h.mu.Lock()
				if caller.callID == id && caller.callState == callRingingOut {
					h.endCall(caller, "no answer")
				}
				h.mu.Unlock()
			}
			return
		case <-tick.C:
			h.mu.Lock()
			peer := h.callPeer(caller)
			if ctx.Err() != nil || caller.callID != id || caller.callState != callRingingOut || peer == nil {
				h.mu.Unlock()
				return
			}
			h.chime(caller, "ring")
			h.chime(peer, "ring")
			h.mu.Unlock()
		}
	}
}

func (h *wsHub) answerCall(callee *wsDevice) {
	if callee.callState != callRingingIn {
		return
	}
	caller := h.callPeer(callee)
	if caller == nil || !h.active(caller) {
		h.endCall(callee, "disconnected")
		return
	}
	caller.callCancel()
	caller.callCancel = nil
	now := time.Now()
	for _, d := range []*wsDevice{caller, callee} {
		d.callState, d.callStarted = callActive, now
		h.push(d, object{"op": "call", "state": "start", "call": d.callID, "muted": false}, false)
		h.render(d, now, false)
	}
}

func (h *wsHub) declineCall(d *wsDevice) {
	if d.callState == callRingingIn {
		h.endCall(d, "declined")
	}
}

func (h *wsHub) endCall(d *wsDevice, reason string) {
	if d.callState == callNone {
		return
	}
	p := h.callPeer(d)
	now := time.Now()
	if p != nil && d.callState == callActive {
		duration := callDuration(now.Sub(d.callStarted))
		dID, pID, dName, pName := d.who.DeviceID, p.who.DeviceID, d.cat.Name, p.cat.Name
		go func() {
			h.b.chat(dID, newChat("card", pName, "Voice call with "+pName+" ("+duration+")", "call"), true)
			h.b.chat(pID, newChat("card", dName, "Voice call with "+dName+" ("+duration+")", "call"), true)
		}()
	}
	for _, side := range []*wsDevice{d, p} {
		if side == nil {
			continue
		}
		if side.callCancel != nil {
			side.callCancel()
		}
		id, answered := side.callID, side.callState == callActive
		side.callState, side.callID, side.callPeer = callNone, 0, ""
		side.callMuted, side.callStarted, side.callSecond, side.callCancel = false, time.Time{}, 0, nil
		if h.active(side) {
			h.push(side, object{"op": "call", "state": "end", "call": id, "reason": reason}, false)
			if answered {
				h.chime(side, "cancel")
			}
			h.clearView(side)
			h.wake(side, now)
			h.render(side, now, false)
		}
	}
}

func (h *wsHub) toggleCallMute(d *wsDevice) {
	if d.callState == callActive {
		d.callMuted = !d.callMuted
		h.push(d, object{"op": "call", "state": "mute", "call": d.callID, "muted": d.callMuted}, false)
		h.render(d, time.Now(), false)
	}
}

func (h *wsHub) relayCallPCM(d *wsDevice, packet []byte) {
	if d.callMuted || len(packet) > callPCMBytes+1 {
		return
	}
	p := h.callPeer(d)
	if p == nil || !h.active(p) || p.callState != callActive {
		return
	}
	c := p.client
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if c.ctx.Err() == nil && len(c.out) < min(6, cap(c.out)) {
		// The read loop transfers ownership of the unmasked packet to the queue.
		packet[0] = pcmPrefix
		c.out <- wsFrame{wsBinary, packet}
	}
}

func callDuration(elapsed time.Duration) string {
	seconds := max(0, int(elapsed/time.Second))
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func callTarget(text string) string {
	verb, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	if strings.EqualFold(verb, "call") || strings.EqualFold(verb, "ring") {
		return strings.TrimSpace(strings.TrimRight(rest, ".!?"))
	}
	return ""
}
