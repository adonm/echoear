package main

import (
	"context"
	"errors"
	"time"
)

func (b *bridge) buildReply(ctx context.Context, who peer, text, source string) (reply, job string, err error) {
	if b.builders == nil {
		return "", "", nil
	}
	intent := parseBuildIntent(text)
	if intent.kind == "" {
		return "", "", nil
	}
	if err = b.parentAllowed(who.DeviceID); err != nil {
		return "", "", err
	}
	if err = ctx.Err(); err != nil {
		return "", "", err
	}
	switch intent.kind {
	case "clarify":
		reply = "Are we making a new app, editing a saved app, or changing my face?"
	case "clarify-app":
		reply = "What would you like the new app to do?"
	case "clarify-face":
		reply = "What would you like me to change about my face?"
	case "cancel":
		cancelled, err := b.builders.cancel(ctx, who.DeviceID, "")
		switch {
		case err != nil:
			reply = "I couldn't cancel the build."
		case cancelled:
			reply = "I cancelled the build."
		default:
			reply = "There isn't a build to cancel."
		}
	default:
		id, err := b.builders.enqueue(ctx, who, intent, text)
		var clientError *apiError
		switch {
		case ctx.Err() != nil:
			return "", "", ctx.Err()
		case errors.As(err, &clientError) && clientError.status == 423:
			return "", "", err
		case errors.As(err, &clientError) && clientError.status < 500:
			reply = clientError.Error()
		case err != nil:
			reply = "I couldn't start the build. Try again."
		case id == "":
			reply = "The builder is busy. Try again in a moment, or say cancel the build."
		default:
			reply, job = "Building it now!", id
		}
	}
	b.chat(who.DeviceID, newChat("user", "You", text, source), true)
	b.chat(who.DeviceID, newChat("cat", b.catFor(who).Name, reply, source), true)
	return reply, job, nil
}

func buildingScene(id string) sceneView {
	view := messageScene("Building", "Building", "I'll tell you when it's ready.", blankCat())
	view.build = id
	view.duringTurn = true
	view.control(bounds{63, 218, 98, 48}, actionTile("listen", "Talk", "", answerPalette))
	view.control(bounds{169, 218, 128, 48}, tile{id: "build-cancel", label: "Cancel build", action: sceneAction{"build-cancel", id}, colors: dangerPalette})
	return view
}

func (h *wsHub) buildAcknowledgement(ctx context.Context, d *wsDevice, turn uint64, reply, job string) {
	view := noticeScene("Build", reply)
	if job != "" {
		view = buildingScene(job)
	}
	h.mu.Lock()
	allowed := h.active(d) && turn == d.turn && h.parentRefresh(d.key, time.Now()) == "" && ctx.Err() == nil
	if allowed {
		d.card = &view
		h.render(d, time.Now(), false)
	}
	h.mu.Unlock()
	if allowed {
		h.sayTurn(ctx, d, turn, reply, nil)
	} else {
		h.finishTurn(d, turn, nil, nil)
	}
}

func (h *wsHub) announceBuild(ctx context.Context, job buildJob) {
	h.mu.Lock()
	d := h.devices[job.device]
	if d == nil || !h.active(d) || h.parentRefresh(d.key, time.Now()) != "" ||
		d.announcing || d.busy || d.queuedVoice != nil || !d.recAt.IsZero() || d.callState != callNone ||
		d.stopAudio != nil || d.awaitAck || d.asleep || d.drawer != "" || d.card != nil && d.card.build != job.id {
		h.mu.Unlock()
		return
	}
	// Claim before playback: reconnects and process restarts never repeat a
	// spoken notification, even if the listener leaves mid-announcement.
	var claimed string
	err := h.b.db.QueryRowContext(ctx, `UPDATE build_jobs SET announced=1
		WHERE id=? AND state=? AND announced=0 RETURNING id`, job.id, job.state).Scan(&claimed)
	if err != nil {
		h.mu.Unlock()
		return
	}
	title := "Build stopped"
	if job.state == "ready" {
		title = "Ready"
	}
	view := messageScene(title, title, job.result, d.cat)
	view.duringTurn = true
	view.control(bounds{129, 218, 102, 48}, tile{id: "build-done", label: "Done", action: sceneAction{"dismiss", ""}, colors: answerPalette})
	d.card = &view
	now := time.Now()
	d.last = now
	h.speakLocal(d, job.result)
	h.render(d, now, false)
	entry := newChat("card", d.cat.Name, job.result, "builder")
	entry.SceneTitle = title
	h.mu.Unlock()
	h.b.chat(job.device, entry, true)
}

func (h *wsHub) cancelBuildCard(d *wsDevice, id string) {
	if h.b.builders == nil || d.card == nil || d.card.build != id {
		return
	}
	card := d.card
	h.mu.Unlock()
	cancelled, _ := h.b.builders.cancel(d.client.ctx, d.who.DeviceID, id)
	h.mu.Lock()
	if cancelled && h.active(d) && d.card == card {
		view := messageScene("Build cancelled", "Build cancelled", "", d.cat)
		view.duringTurn = true
		view.control(bounds{129, 218, 102, 48}, tile{id: "build-done", label: "Done", action: sceneAction{"dismiss", ""}, colors: answerPalette})
		d.card = &view
		h.render(d, time.Now(), false)
	}
}
