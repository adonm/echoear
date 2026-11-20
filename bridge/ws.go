package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const wsConnections = 64
const idleSleep = 120 * time.Second

var phoneID = regexp.MustCompile(`^phone-[0-9a-f]{4}$`)

type wsEvent struct {
	Event     string `json:"ev"`
	Role      string `json:"role"`
	DeviceID  string `json:"device_id"`
	Character string `json:"character"`
	Firmware  string `json:"firmware"`
	ID        string `json:"id"`
	X         int    `json:"x"`
	Y         int    `json:"y"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Text      string `json:"text"`
	State     string `json:"state"`
	Audio     uint64 `json:"audio"`
	Cancelled bool   `json:"cancelled"`
	wsStatus
}

type wsStatus struct {
	Battery *int  `json:"battery_pct"`
	MV      *int  `json:"battery_mv"`
	Charge  *bool `json:"charging"`
	Volume  *int  `json:"volume"`
	RSSI    *int  `json:"rssi"`
}

type wsClient struct {
	wire   *websocket
	ctx    context.Context
	cancel context.CancelFunc
	out    chan wsFrame
	role   string
	watch  string
	device *wsDevice
	scope  *virtualClient
}

type wsDevice struct {
	client   *wsClient
	who      peer
	key      string
	firmware string
	theme    string
	vol      int
	pct      int
	mV       int
	rssi     int
	charging bool

	scene       sceneView
	view        *sceneView
	pending     *sceneView
	drawer      bool
	rev         uint64
	pose        catPose
	drag        string
	moodAt      time.Time
	last        time.Time
	meter       time.Time
	pet         time.Time
	asleep      bool
	customScene bool

	recAt         time.Time
	recOpen       bool
	recTarget     string
	mic           []byte
	turn          uint64
	cancel        context.CancelFunc
	busy          bool
	agentSetScene bool
	agentSpoke    bool

	audio      uint64
	stopAudio  context.CancelFunc
	ttsStarted bool
	awaitAck   bool
	audioUntil time.Time
}

func (d *wsDevice) virtual() bool {
	return virtualID.MatchString(d.key) || phoneID.MatchString(d.key)
}

// The hub lock orders state changes and outbound frames. Socket writes and
// backend requests never run under it.
type wsHub struct {
	b          *bridge
	mu         sync.Mutex
	conns      sync.WaitGroup
	clients    map[*wsClient]struct{}
	devices    map[string]*wsDevice
	events     map[string]*eventRing
	httpSpeech map[string]*atomic.Bool
	closed     bool
	peers      string
}

func newWSHub(b *bridge) *wsHub {
	h := &wsHub{b: b, clients: make(map[*wsClient]struct{}), devices: make(map[string]*wsDevice),
		events: make(map[string]*eventRing), httpSpeech: make(map[string]*atomic.Bool)}
	go h.animate(b.root)
	return h
}

func (h *wsHub) shutdown() {
	h.mu.Lock()
	if !h.closed {
		h.closed = true
		for c := range h.clients {
			c.cancel()
		}
	}
	h.mu.Unlock()
	h.conns.Wait()
}

func (h *wsHub) serve(w http.ResponseWriter, r *http.Request) error {
	if !tokenMatches(r.Header.Values("X-EchoEar-Token"), h.b.token) && !tokenMatches(r.URL.Query()["token"], h.b.token) {
		return problem(401, "Unauthorized")
	}
	return h.serveScope(w, r, nil)
}

func (h *wsHub) serveScope(w http.ResponseWriter, r *http.Request, scope *virtualClient) error {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
			return problem(403, "Unexpected WebSocket origin")
		}
	}
	h.mu.Lock()
	full := h.closed || len(h.clients) >= wsConnections
	h.mu.Unlock()
	if full {
		return problem(503, "WebSocket hub unavailable")
	}
	wire, err := upgradeWebSocket(w, r)
	if err != nil || wire == nil {
		return err
	}
	ctx, cancel := context.WithCancel(r.Context())
	c := &wsClient{wire: wire, ctx: ctx, cancel: cancel, out: make(chan wsFrame, 192), scope: scope}
	h.mu.Lock()
	if closed := h.closed; closed || len(h.clients) >= wsConnections {
		h.mu.Unlock()
		cancel()
		code := uint16(1013)
		if closed {
			code = 1001
		}
		_ = wire.write(wsCloseFrame(code))
		wire.conn.Close()
		return nil
	}
	h.clients[c] = struct{}{}
	h.conns.Add(1)
	h.mu.Unlock()
	go c.write()
	defer h.conns.Done()
	defer func() {
		c.wire.conn.Close()
		cancel()
		h.detach(c)
	}()
	for {
		f, err := wire.read()
		if err != nil || c.ctx.Err() != nil {
			return nil
		}
		switch f.op {
		case wsText:
			var ev wsEvent
			if json.Unmarshal(f.data, &ev) == nil {
				h.handle(c, ev)
			}
		case wsBinary:
			h.micChunk(c, f.data)
		}
	}
}

func (c *wsClient) write() {
	defer c.wire.conn.Close()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
		case f := <-c.out:
			if c.wire.write(f) != nil {
				return
			}
		case <-ping.C:
			if c.wire.write(wsFrame{op: wsPing}) != nil {
				return
			}
		}
	}
	_ = c.wire.write(wsCloseFrame(1001))
}

func (c *wsClient) enqueue(f wsFrame) bool {
	if c.ctx.Err() != nil {
		return false
	}
	select {
	case c.out <- f:
		return true
	default:
		c.cancel()
		c.wire.conn.Close()
		return false
	}
}

func textFrame(v object) wsFrame {
	raw, _ := json.Marshal(v)
	return wsFrame{wsText, raw}
}

func (h *wsHub) push(d *wsDevice, v object, mirror bool) {
	f := textFrame(v)
	d.client.enqueue(f)
	if mirror {
		for c := range h.clients {
			if c.watch == d.key {
				c.enqueue(f)
			}
		}
	}
}

func (h *wsHub) active(d *wsDevice) bool { return h.devices[d.key] == d && d.client.ctx.Err() == nil }

func (h *wsHub) detach(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c)
	if d := c.device; d != nil && h.devices[d.key] == d {
		delete(h.devices, d.key)
		h.cancelTurn(d)
		h.stopPlayback(d, false)
		h.broadcastPeers()
	}
}

func (h *wsHub) disconnectDevice(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.devices[id]; d != nil {
		d.client.cancel()
		delete(h.devices, id)
		h.cancelTurn(d)
		h.stopPlayback(d, false)
		h.broadcastPeers()
	}
}

func (h *wsHub) peerSnapshot(sender string) ([]peer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.combinedPeers(sender)
}

func (b *bridge) peerSnapshot(sender string) ([]peer, error) {
	if b.ws == nil {
		return b.peers.snapshot(sender)
	}
	return b.ws.peerSnapshot(sender)
}

func (h *wsHub) combinedPeers(sender string) ([]peer, error) {
	stored, err := h.b.peers.snapshot(sender)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(stored))
	for i, p := range stored {
		index[strings.ToLower(p.DeviceID)] = i
	}
	now := time.Now().Unix()
	for _, d := range h.devices {
		if !h.active(d) || strings.EqualFold(sender, d.who.DeviceID) {
			continue
		}
		p := d.who
		if d.virtual() {
			p.IP = ""
		}
		p.LastSeen = now
		if i, ok := index[d.key]; ok {
			p.IP, p.Port = stored[i].IP, stored[i].Port
			stored[i] = p
		} else {
			stored = append(stored, p)
		}
	}
	slices.SortFunc(stored, func(a, b peer) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return stored, nil
}

func (h *wsHub) peersChanged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcastPeers()
}

func (h *wsHub) peerFrame() wsFrame {
	list, _ := h.combinedPeers("")
	p := publicPeers(list)
	for i, entry := range p {
		if d := h.devices[strings.ToLower(list[i].DeviceID)]; d != nil && h.active(d) {
			entry["ws"], entry["firmware"] = true, d.firmware
		} else {
			entry["ws"] = false
		}
	}
	return textFrame(object{"op": "peers", "devices": p})
}

func (h *wsHub) scopedPeerFrame() wsFrame {
	list, _ := h.combinedPeers("")
	devices := make([]object, 0, len(list))
	now := time.Now().Unix()
	for _, p := range list {
		if now-p.LastSeen <= 180 {
			devices = append(devices, object{"device_id": p.DeviceID, "character": p.Character, "online": true})
		}
	}
	return textFrame(object{"op": "peers", "devices": devices})
}

func (h *wsHub) broadcastPeers() {
	if len(h.clients) == 0 {
		h.peers = ""
		return
	}
	f := h.peerFrame()
	if string(f.data) != h.peers {
		h.peers = string(f.data)
		var scoped wsFrame
		for c := range h.clients {
			switch {
			case c.role != "web":
			case c.scope == nil:
				c.enqueue(f)
			default:
				if scoped.data == nil {
					scoped = h.scopedPeerFrame()
				}
				c.enqueue(scoped)
			}
		}
		for _, d := range h.devices {
			if d.drawer {
				h.render(d, time.Now(), false)
			}
		}
	}
}

func (h *wsHub) hello(c *wsClient, ev wsEvent, now time.Time) {
	phone := phoneID.MatchString(ev.DeviceID)
	var v virtualClient
	if c.scope != nil {
		ev.Role, ev.DeviceID = "web", c.scope.ID
		v = *c.scope
	} else if virtualID.MatchString(strings.ToLower(ev.DeviceID)) {
		var ok bool
		v, ok = h.b.virtualByID(ev.DeviceID)
		if !ok {
			c.enqueue(textFrame(object{"op": "error", "status": 404, "error": "Virtual client not found"}))
			return
		}
		ev.Role = "web"
	}
	if ev.Role != "device" && ev.Role != "web" ||
		ev.Role == "device" && !deviceIDRE.MatchString(ev.DeviceID) ||
		ev.Role == "web" && ev.DeviceID != "" && !deviceIDRE.MatchString(ev.DeviceID) {
		return
	}
	if ev.Role == "web" && !phone && v.ID == "" {
		c.role = "web"
		c.enqueue(h.peerFrame())
		return
	}
	character := strings.TrimSpace(ev.Character)
	if v.ID != "" {
		character = v.Character
	}
	if !characterRE.MatchString(character) {
		return
	}
	host, _, _ := net.SplitHostPort(c.wire.conn.RemoteAddr().String())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	ip, _ := lanIP(host, true) // Advertised IP never replaces an HTTP registration.
	d := &wsDevice{client: c, key: strings.ToLower(ev.DeviceID), who: peer{DeviceID: ev.DeviceID, Character: character, IP: ip},
		firmware: shortText(ev.Firmware, 24, false), theme: v.Theme, pct: -1, vol: 80, last: now, moodAt: now}
	d.status(ev.wsStatus)
	if old := h.devices[d.key]; old != nil {
		h.cancelTurn(old)
		h.stopPlayback(old, true)
		old.client.cancel()
		d.rev = old.rev
	}
	h.devices[d.key], c.device, c.role = d, d, ev.Role
	h.render(d, now, true)
	h.push(d, object{"op": "volume", "level": d.vol}, true)
	h.push(d, object{"op": "power", "brightness": 85}, true)
	previous := h.peers
	h.broadcastPeers()
	if c.role == "web" && previous == h.peers {
		if c.scope != nil {
			c.enqueue(h.scopedPeerFrame())
		} else {
			c.enqueue(h.peerFrame())
		}
	}
	if h.b.worker != nil {
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, ttsWait)
			defer cancel()
			h.b.warmCharacter(ctx, character)
		}()
	}
}

func (d *wsDevice) status(s wsStatus) {
	if s.Battery != nil {
		d.pct = min(100, max(-1, *s.Battery))
	}
	if s.MV != nil {
		d.mV = min(6000, max(0, *s.MV))
	}
	if s.Charge != nil {
		d.charging = *s.Charge
	}
	if s.RSSI != nil {
		d.rssi = min(0, max(-127, *s.RSSI))
	}
	if s.Volume != nil {
		d.vol = min(100, max(0, *s.Volume))
	}
}

func (h *wsHub) render(d *wsDevice, now time.Time, full bool) {
	mood := "idle"
	switch {
	case d.recOpen || !d.recAt.IsZero():
		mood = "listening"
	case d.ttsStarted || d.awaitAck:
		mood = "speaking"
	case d.busy:
		mood = "thinking"
	case d.asleep:
		mood = "sleepy"
	}
	changed := mood != d.pose.mood
	if changed {
		d.moodAt = now
	}
	if changed && d.virtual() {
		h.push(d, object{"op": "mode", "mode": mood}, false)
	}
	pose := poseFrame(now, d.moodAt, d.last, mood, d.pct, d.vol, d.charging, now.Before(d.meter))
	pose.blink = pose.blink || now.Before(d.pet)
	var view sceneView
	switch {
	case d.drawer && mood == "idle":
		peers, _ := h.combinedPeers(d.who.DeviceID)
		peers = slices.DeleteFunc(peers, func(p peer) bool { return now.Unix()-p.LastSeen > 180 })
		view = drawerScene(d.who.Character, peers)
	case d.view != nil && (d.customScene || mood != "sleepy" && mood != "listening"):
		view = *d.view
	default:
		theme := d.theme
		if theme == "" {
			theme = d.who.Character
		}
		view = catScene(theme, pose)
	}
	d.pose = pose
	d.rev++
	v := view.full(d.rev)
	if !full && d.rev > 1 {
		v = sceneDiff(d.scene, view, d.rev)
	}
	d.scene = view
	if v == nil {
		d.rev--
		return
	}
	h.push(d, v, true)
}

func (h *wsHub) wake(d *wsDevice, now time.Time) {
	d.last = now
	if d.asleep {
		d.asleep = false
		h.push(d, object{"op": "power", "brightness": 85}, true)
	}
}

func (h *wsHub) animate(ctx context.Context) {
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	sincePeers := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.mu.Lock()
			if h.closed {
				h.mu.Unlock()
				return
			}
			for _, d := range h.devices {
				switch {
				case !d.recAt.IsZero() && now.Sub(d.recAt) > 65*time.Second:
					h.clearRecording(d)
					h.push(d, object{"op": "rec", "action": "stop", "target": ""}, false)
					h.push(d, object{"op": "chime", "kind": "cancel"}, false)
					h.render(d, now, false)
				case d.awaitAck && now.After(d.audioUntil.Add(4*time.Second)):
					h.playbackDone(d, now)
				case !d.asleep && !d.busy && d.recAt.IsZero() && !d.ttsStarted && !d.awaitAck && now.Sub(d.last) >= idleSleep:
					d.asleep = true
					h.push(d, object{"op": "power", "brightness": 12}, true)
					h.render(d, now, false)
				case !d.drawer && (d.view == nil || d.asleep || !d.recAt.IsZero()):
					p := poseFrame(now, d.moodAt, d.last, d.pose.mood, d.pct, d.vol, d.charging, now.Before(d.meter))
					p.blink = p.blink || now.Before(d.pet)
					if p != d.pose {
						h.render(d, now, false)
					}
				}
			}
			if now.Sub(sincePeers) > 15*time.Second {
				h.broadcastPeers()
				sincePeers = now
			}
			h.mu.Unlock()
		}
	}
}

func (h *wsHub) handle(c *wsClient, ev wsEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return
	}
	if c.scope != nil && ev.DeviceID != "" && ev.DeviceID != c.scope.ID {
		c.enqueue(textFrame(object{"op": "error", "status": 403, "error": "Device out of scope"}))
		return
	}
	now := time.Now()
	if c.role == "" {
		if ev.Event == "hello" {
			h.hello(c, ev, now)
		}
		return
	}
	d := c.device
	if d == nil {
		if ev.Event == "watch" && c.role == "web" {
			if ev.DeviceID == "" {
				c.watch = ""
				return
			}
			c.watch = strings.ToLower(ev.DeviceID)
			if d := h.devices[c.watch]; d != nil && h.active(d) {
				c.enqueue(textFrame(d.scene.full(d.rev)))
				c.enqueue(textFrame(object{"op": "volume", "level": d.vol}))
				brightness := 85
				if d.asleep {
					brightness = 12
				}
				c.enqueue(textFrame(object{"op": "power", "brightness": brightness}))
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
		c.enqueue(textFrame(d.scene.full(d.rev)))
		return
	}
	switch ev.Event {
	case "status":
		prev := d.vol
		d.status(ev.wsStatus)
		if d.vol != prev {
			h.push(d, object{"op": "volume", "level": d.vol}, true)
		}
		h.render(d, now, false)
	case "tap", "drag", "up":
		h.pointer(d, ev, now)
	case "head":
		switch ev.Action {
		case "pet":
			h.wake(d, now)
			d.pet = now.Add(300 * time.Millisecond)
			h.push(d, object{"op": "chime", "kind": "tap"}, false)
			h.emit(d.who.DeviceID, "head_pet", object{})
			h.render(d, now, false)
		case "vol_up", "vol_down":
			v := d.vol + 5
			if ev.Action == "vol_down" {
				v = d.vol - 5
			}
			if ev.Volume != nil {
				v = *ev.Volume
			}
			h.volume(d, v, now)
		}
	case "rec_start":
		if target, ok := recordTarget(ev.Target); ok && !d.recOpen {
			if target == "" {
				target = d.recTarget
			}
			h.startRecording(d, target, now, false)
		}
	case "rec_end":
		if !d.recAt.IsZero() {
			h.endRecording(d, ev, now)
		}
	case "text":
		text := strings.TrimSpace(ev.Text)
		if text != "" && len(text) <= 4096 {
			if h.emit(d.who.DeviceID, "speech", object{"text": text, "source": "text"}) {
				return
			}
			h.startTurn(d, now, false, func(ctx context.Context, id uint64) {
				h.runTurn(ctx, d, id, text, nil)
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
			h.playbackDone(d, now)
		}
	}
}

func (h *wsHub) pointer(d *wsDevice, ev wsEvent, now time.Time) {
	if ev.X < 0 || ev.X > 359 || ev.Y < 0 || ev.Y > 359 || len(ev.ID) > 24 {
		return
	}
	wasAsleep := d.asleep
	h.wake(d, now)
	if wasAsleep {
		h.render(d, now, false)
		return
	}
	if ev.Event != "tap" {
		if ev.Event == "drag" || d.drag != "" {
			id := ev.ID
			if d.drag != "" {
				id = d.drag
			}
			if key, a := d.scene.action(id, ev.X, ev.Y, "drag"); a.kind == "volume" {
				d.drag = key
				h.volume(d, (ev.X-123)*100/114, now)
			}
		}
		if ev.Event == "up" {
			d.drag = ""
		}
		return
	}
	key, a := d.scene.action(ev.ID, ev.X, ev.Y, "tap")
	if key != "" || d.customScene {
		waiting := h.emit(d.who.DeviceID, "tap", object{"id": key, "x": ev.X, "y": ev.Y})
		if d.customScene && key != "" && waiting {
			if a.kind == "cat" {
				d.drawer, d.view, d.pending, d.customScene, d.agentSetScene = false, nil, nil, false, d.busy
				h.render(d, now, true)
			} else {
				h.push(d, object{"op": "chime", "kind": "tap"}, false)
			}
			return
		}
	}
	switch a.kind {
	case "drawer":
		h.push(d, object{"op": "chime", "kind": "tap"}, false)
		d.drawer = true
		h.render(d, now, false)
	case "cat":
		h.cancelTurn(d)
		h.stopPlayback(d, true)
		d.drawer, d.view, d.pending, d.customScene = false, nil, nil, false
		h.render(d, now, true)
	case "new":
		h.startTurn(d, now, false, func(ctx context.Context, id uint64) { h.resetTurn(ctx, d, id) })
	case "listen", "reply", "reply_to":
		if !d.recAt.IsZero() {
			h.push(d, object{"op": "rec", "action": "stop", "target": d.recTarget}, false)
		} else {
			h.startRecording(d, a.value, now, true)
		}
	case "tap":
		h.push(d, object{"op": "chime", "kind": "tap"}, false)
		d.view = d.scene.hint(a.value)
		h.startTurn(d, now, true, func(ctx context.Context, id uint64) { h.sayTurn(ctx, d, id, a.value, nil) })
	case "ask":
		h.push(d, object{"op": "chime", "kind": "think"}, false)
		hint := d.scene.feedback[key]
		if hint == "" {
			hint = "Asking…"
		}
		d.view = d.scene.hint(hint)
		h.startTurn(d, now, true, func(ctx context.Context, id uint64) { h.runTurn(ctx, d, id, a.value, nil) })
	case "event":
		h.push(d, object{"op": "chime", "kind": "tap"}, false)
	}
}

func (h *wsHub) volume(d *wsDevice, level int, now time.Time) {
	h.wake(d, now)
	level = min(100, max(0, level))
	if level != d.vol {
		d.vol = level
		h.push(d, object{"op": "volume", "level": level}, true)
	}
	d.meter = now.Add(1800 * time.Millisecond)
	h.render(d, now, false)
}
