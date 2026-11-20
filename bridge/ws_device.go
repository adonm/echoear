package main

import (
	"bytes"
	"context"
	"log"
	"maps"
	"math"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

const idleSleep = 120 * time.Second

var phoneID = regexp.MustCompile(`^phone-[0-9a-f]{4}$`)

type wsStatus struct {
	Battery    *int     `json:"battery_pct"`
	MV         *int     `json:"battery_mv"`
	Charge     *bool    `json:"charging"`
	RSSI       *int     `json:"rssi,omitempty"`
	WiFi       *int     `json:"wifi_rssi"`
	HasBattery *bool    `json:"has_battery"`
	Volume     *int     `json:"volume"`
	FreeHeap   *uint64  `json:"free_heap"`
	MinHeap    *uint64  `json:"min_free_heap"`
	FreePSRAM  *uint64  `json:"free_psram"`
	Uptime     *uint64  `json:"uptime_s"`
	CPU        *float64 `json:"cpu_pct"`
	Partition  string   `json:"running_partition,omitempty"`
}

type wsDevice struct {
	client     *wsClient
	who        peer
	key        string
	firmware   string
	screen     screenGeometry
	board      string
	theme      string
	voice      string
	announcing bool
	cat        catState
	vol        int
	autoVol    bool
	ambientEMA float64
	hasAmbient bool
	pct        int
	charging   bool
	telemetry  wsStatus
	frame      *diagFrame
	svg        svgScreen
	svgWire    wsFrame
	displayApp *appInstance
	renderGen  uint64
	rendering  bool
	renderNext *renderRequest
	app        *appInstance
	appView    svgScreen
	appGen     uint64

	scene    sceneView
	view     *sceneView
	pending  *sceneView
	card     *sceneView
	drawer   string
	peerPage int
	statsAt  time.Time
	rev      uint64
	pose     catPose
	moodAt   time.Time
	last     time.Time
	meter    time.Time
	pet      time.Time
	asleep   bool

	recAt          time.Time
	recOpen        bool
	recTarget      string
	recStopping    bool
	recStopTimer   *time.Timer
	recIgnoreStart bool
	mic            []byte
	turn           uint64
	cancel         context.CancelFunc
	busy           bool
	agentSetScene  bool

	audio      uint64
	stopAudio  context.CancelFunc
	ttsStarted bool
	awaitAck   bool
	audioUntil time.Time
}

func webDeviceID(id string) bool { return virtualID.MatchString(id) || phoneID.MatchString(id) }

func (d *wsDevice) virtual() bool { return d.client.role == "web" || webDeviceID(d.key) }

func (c *wsClient) ping(now time.Time) wsFrame {
	c.pingMu.Lock()
	c.pingSentAt = now
	c.pingMu.Unlock()
	return wsFrame{wsPing, le.AppendUint64(nil, uint64(now.UnixNano()))}
}

func (c *wsClient) pong(data []byte) {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	if len(data) == 8 && !c.pingSentAt.IsZero() && le.Uint64(data) == uint64(c.pingSentAt.UnixNano()) {
		c.rtt = new(milliseconds(time.Since(c.pingSentAt)))
		c.pingSentAt = time.Time{}
	}
}

func (c *wsClient) pingRTT() *float64 {
	c.pingMu.Lock()
	defer c.pingMu.Unlock()
	return c.rtt
}

func (h *wsHub) peerSnapshot(sender string) ([]peer, error) {
	stored, err := h.b.peers.snapshot(sender)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	online := h.onlinePeers(sender)
	h.mu.Unlock()
	list := make([]peer, 0, len(stored)+len(online))
	for _, p := range stored {
		if _, connected := online[strings.ToLower(p.DeviceID)]; !connected {
			p.Character = h.b.catFor(p).Name
			list = append(list, p)
		}
	}
	for _, p := range online {
		list = append(list, p.peer)
	}
	slices.SortFunc(list, func(a, b peer) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return list, nil
}

func (b *bridge) peerSnapshot(sender string) ([]peer, error) {
	if b.ws == nil {
		list, err := b.peers.snapshot(sender)
		for i := range list {
			list[i].Character = b.catFor(list[i]).Name
		}
		return list, err
	}
	return b.ws.peerSnapshot(sender)
}

type peerDisplay struct {
	peer
	cat      catLook
	auto     bool
	online   bool
	firmware string
}

func (h *wsHub) onlinePeers(sender string) map[string]peerDisplay {
	out := make(map[string]peerDisplay, len(h.devices))
	for _, d := range h.devices {
		if !h.active(d) || strings.EqualFold(sender, d.who.DeviceID) {
			continue
		}
		p := d.who
		p.Character = d.cat.Name
		if d.virtual() {
			p.IP = ""
		}
		out[d.key] = peerDisplay{p, d.cat.catLook, d.autoVol, true, d.firmware}
	}
	return out
}

func (h *wsHub) peerFrame(scoped bool) wsFrame {
	online := h.onlinePeers("")
	list := make([]peerDisplay, 0, len(h.storedPeers)+len(online))
	if !scoped {
		for _, p := range h.storedPeers {
			if _, ok := online[strings.ToLower(p.DeviceID)]; !ok {
				list = append(list, p)
			}
		}
	}
	for _, p := range online {
		list = append(list, p)
	}
	slices.SortFunc(list, func(a, b peerDisplay) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	devices := make([]object, 0, len(list))
	for _, p := range list {
		if scoped {
			devices = append(devices, object{"device_id": p.DeviceID, "character": p.Character, "online": true})
			continue
		}
		entry := object{"device_id": p.DeviceID, "character": p.Character, "ip": p.IP,
			"port": p.Port, "last_seen": p.LastSeen, "online": p.online,
			"ws": p.online, "cat": p.cat, "auto_volume": p.auto}
		if p.online {
			entry["firmware"] = p.firmware
		}
		devices = append(devices, entry)
	}
	return textFrame(object{"op": "peers", "devices": devices})
}

func (h *wsHub) broadcastPeers() {
	h.broadcastPeerFrames()
	for c := range h.clients {
		if c.role == "web" && c.scope == nil {
			h.refreshNeeded = true
			if !h.refreshing {
				h.refreshing = true
				go h.refreshStoredPeers()
			}
			return
		}
	}
}

func (c *wsClient) pushPeers(f wsFrame) {
	if !bytes.Equal(c.lastPeers.data, f.data) {
		c.lastPeers = f
		c.enqueue(f)
	}
}

func (h *wsHub) refreshStoredPeers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for h.refreshNeeded && !h.closed {
		h.refreshNeeded = false
		h.mu.Unlock()
		list, err := h.b.peers.snapshot("")
		stored := make([]peerDisplay, 0, len(list))
		for _, p := range list {
			cat := h.b.catFor(p)
			p.Character = cat.Name
			auto, _ := h.b.volumePrefs(p.DeviceID)
			stored = append(stored, peerDisplay{peer: p, cat: cat.catLook, auto: auto})
		}
		h.mu.Lock()
		if err == nil && !h.closed && !h.refreshNeeded {
			h.storedPeers = stored
			h.broadcastPeerFrames()
		}
	}
	h.refreshing = false
}

func (h *wsHub) broadcastPeerFrames() {
	if len(h.clients) == 0 {
		h.peers = ""
		return
	}
	f := h.peerFrame(false)
	if string(f.data) != h.peers {
		h.peers = string(f.data)
		var scoped wsFrame
		for c := range h.clients {
			switch {
			case c.role != "web" || c.device != nil && c.device.announcing:
			case c.scope == nil:
				c.pushPeers(f)
			default:
				if scoped.data == nil {
					scoped = h.peerFrame(true)
				}
				c.pushPeers(scoped)
			}
		}
		for _, d := range h.devices {
			if d.drawer == "intercom" {
				h.render(d, time.Now(), false)
			}
		}
	}
}

// Called under the hub lock, after committing the cat and profile changes.
func (h *wsHub) catChanged(id string, cat catState, profilesChanged bool) {
	key := strings.ToLower(id)
	if d := h.devices[key]; d != nil && h.active(d) {
		d.cat = cat
		d.voice = cat.Voice
		if !h.b.voices.allowed(d.voice) {
			d.voice = h.b.voices.defaultFor(d.voiceTheme())
		}
		h.clearView(d)
		d.agentSetScene = d.busy
		now := time.Now()
		h.render(d, now, true)
	}
	f := catFrame(id, cat, cat.Voice)
	for c := range h.clients {
		if c.role == "web" && (c.scope == nil || c.scope.ID == key) {
			c.enqueue(f)
		}
	}
	h.broadcastPeers()
	if profilesChanged {
		h.savedCatsChanged()
	}
}

func catFrame(id string, cat catState, voice string) wsFrame {
	return textFrame(object{"op": "cat", "device_id": id, "character": cat.Name, "cat": cat.catLook, "voice": voice})
}

func (h *wsHub) savedCatsChanged() {
	f := textFrame(object{"op": "saved_cats"})
	for c := range h.clients {
		if c.role == "web" && c.scope == nil {
			c.enqueue(f)
		}
	}
}

func (h *wsHub) catsReassigned(from, to string) {
	for _, id := range []string{from, to} {
		f := textFrame(object{"op": "cat_reassigned", "device_id": id})
		for c := range h.clients {
			if c.role == "web" && (c.scope == nil || c.scope.ID == id) {
				c.enqueue(f)
			}
		}
	}
}

func (h *wsHub) hello(c *wsClient, ev wsEvent, now time.Time) {
	if ev.Role == "device" && ev.Proto < 2 {
		c.enqueue(textFrame(object{"op": "error", "status": 426, "error": "Device firmware upgrade required: WebSocket protocol 2 or newer"}))
		return
	}
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
		ev.Role == "device" && (!deviceIDRE.MatchString(ev.DeviceID) || phone) ||
		ev.Role == "web" && ev.DeviceID != "" && !deviceIDRE.MatchString(ev.DeviceID) {
		return
	}
	if ev.Role == "web" && !phone && v.ID == "" {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closed || c.ctx.Err() != nil {
			return
		}
		c.role = "web"
		c.pushPeers(h.peerFrame(false))
		h.broadcastPeers()
		return
	}
	screen, board := roundScreen, ""
	if ev.Role == "device" {
		board = "esp-vocat"
		if ev.Width != nil {
			screen.width = *ev.Width
		}
		if ev.Height != nil {
			screen.height = *ev.Height
		}
		if ev.Shape != "" {
			screen.shape = ev.Shape
		}
		if ev.Board != "" {
			board = ev.Board
		}
		// Bound native frames by the v2 maximum (360×360 RGB565).
		if screen.width < 1 || screen.width > screenSize || screen.height < 1 || screen.height > screenSize ||
			screen.shape != "round" && screen.shape != "rect" || board != "esp-vocat" && board != "esp-box-3" {
			c.enqueue(textFrame(object{"op": "error", "status": 400, "error": "Invalid display capabilities"}))
			return
		}
	}
	character := strings.TrimSpace(ev.Character)
	if v.ID != "" {
		character = v.Character
	} else if character == "" && ev.Role == "device" {
		character = "New Cat"
		if h.b.peers.store != nil {
			if prior, ok, _ := h.b.peers.get(ev.DeviceID); ok {
				character = prior.Character
			}
		}
	}
	if !characterRE.MatchString(character) {
		return
	}
	host, _, _ := net.SplitHostPort(c.wire.conn.RemoteAddr().String())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	who := peer{DeviceID: ev.DeviceID, Character: character, LastSeen: now.Unix()}
	if ev.Role == "device" {
		who.IP, who.Port = host, 80
		if ev.Port != nil {
			if *ev.Port < 1 || *ev.Port > 65535 {
				c.enqueue(textFrame(object{"op": "error", "status": 400, "error": "Invalid device port"}))
				return
			}
			who.Port = *ev.Port
		}
		if h.b.peers.store != nil {
			if err := h.b.peers.register(who); err != nil {
				c.enqueue(textFrame(object{"op": "error", "status": 502, "error": "Cannot remember device"}))
				return
			}
		}
	}
	d := &wsDevice{client: c, key: strings.ToLower(ev.DeviceID), who: who,
		firmware: shortText(ev.Firmware, 24, false), screen: screen, board: board,
		theme: v.Theme, pct: -1, last: now, moodAt: now, announcing: true}
	d.cat = h.b.catFor(d.who)
	d.voice = h.b.voices.get(d.who.DeviceID, d.voiceTheme())
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	if ev.Role == "device" {
		d.telemetry.HasBattery = new(board == "esp-vocat")
	}
	d.status(ev.wsStatus)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || c.ctx.Err() != nil {
		return
	}
	if old := h.devices[d.key]; old != nil {
		h.dropApp(old)
		h.clearRecording(old)
		h.cancelTurn(old)
		h.stopPlayback(old, true)
		old.client.cancel()
		d.rev = old.rev
	}
	h.devices[d.key], c.device, c.role = d, d, ev.Role
	h.render(d, now, true)
}

func (h *wsHub) announce(d *wsDevice) {
	d.announcing = false
	c := d.client
	if c.role == "web" {
		c.enqueue(catFrame(d.who.DeviceID, d.cat, d.voice))
	}
	h.push(d, d.volumeFrame(false), true)
	h.push(d, object{"op": "power", "brightness": d.brightness()}, true)
	h.broadcastPeers()
	if c.role == "web" {
		c.pushPeers(h.peerFrame(c.scope != nil))
	}
	if h.b.worker != nil {
		name, voice := d.cat.Name, d.voice
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, ttsWait)
			defer cancel()
			h.b.warmCharacter(ctx, voice, name)
		}()
	}
}

func (d *wsDevice) status(s wsStatus) {
	t := &d.telemetry
	if s.HasBattery != nil {
		t.HasBattery = s.HasBattery
		if !*s.HasBattery {
			d.pct, d.charging = -1, false
			t.Battery, t.MV, t.Charge = nil, nil, nil
		}
	}
	if s.Battery != nil {
		d.pct = min(100, max(-1, *s.Battery))
		if d.pct >= 0 {
			t.Battery, t.HasBattery = new(d.pct), new(true)
		}
	}
	if s.MV != nil {
		t.MV = new(min(6000, max(0, *s.MV)))
	}
	if s.Charge != nil {
		d.charging = *s.Charge
		t.Charge = s.Charge
	}
	if s.WiFi == nil {
		s.WiFi = s.RSSI
	}
	if s.WiFi != nil && *s.WiFi < 0 {
		t.WiFi = new(max(-127, *s.WiFi))
	}
	if s.Volume != nil {
		t.Volume = new(min(100, max(0, *s.Volume)))
	}
	if s.FreeHeap != nil {
		t.FreeHeap = s.FreeHeap
	}
	if s.MinHeap != nil {
		t.MinHeap = s.MinHeap
	}
	if s.FreePSRAM != nil {
		t.FreePSRAM = s.FreePSRAM
	}
	if s.Uptime != nil {
		t.Uptime = s.Uptime
	}
	if s.CPU != nil && !math.IsNaN(*s.CPU) {
		t.CPU = new(min(100., max(0., *s.CPU)))
	}
	if s.Partition != "" {
		t.Partition = shortText(s.Partition, 24, false)
	}
}

func (d *wsDevice) voiceTheme() string {
	if d.theme != "" {
		return d.theme
	}
	return deviceTheme(d.who.Character)
}

type renderRequest struct {
	view   sceneView
	cat    *catState
	pose   catPose
	app    *appInstance
	screen svgScreen
	full   bool
}

func (h *wsHub) render(d *wsDevice, now time.Time, full bool) {
	mood := "idle"
	switch {
	case d.asleep:
		mood = "sleepy"
	case d.recOpen || !d.recAt.IsZero():
		mood = "listening"
	case d.ttsStarted || d.awaitAck:
		mood = "speaking"
	case d.busy:
		mood = "thinking"
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
	d.pose = pose
	r := renderRequest{full: full || d.renderNext != nil && d.renderNext.full}
	switch {
	case d.drawer != "" && mood == "idle":
		switch d.drawer {
		case "volume":
			r.view = volumeScene(d.autoVol, d.vol)
		case "voice":
			r.view = voiceScene(d.voice)
		case "intercom":
			choices := []menuPeer{{"all", "Everyone", "#FFADAC", "#FFD68B"}}
			for _, other := range h.devices {
				if other != d && h.active(other) {
					choices = append(choices, menuPeer{other.who.DeviceID, other.cat.Name, other.cat.FaceBG, other.cat.AccentColor})
				}
			}
			slices.SortFunc(choices[1:], func(a, b menuPeer) int { return strings.Compare(a.id, b.id) })
			d.peerPage = min(max(0, d.peerPage), (len(choices)-1)/4)
			r.view = intercomSceneMenu(choices, d.peerPage)
		case "stats":
			if d.client.scope == nil {
				d.statsAt = now
				r.view = h.statsScene(d)
				break
			}
			fallthrough
		default:
			r.view = menuHome(d.voice, d.autoVol, d.vol, d.client.scope != nil)
		}
	case mood == "listening":
		r.cat, r.pose = new(d.cat), pose
	case d.card != nil && (mood == "idle" || mood == "speaking" || mood != "sleepy" && d.card.duringTurn):
		r.view = *d.card
	case d.app != nil && mood != "sleepy":
		r.app, r.screen = d.app, d.appView
		r.view.title = d.app.spec.Title
	case d.view != nil && mood != "sleepy":
		r.view = *d.view
	default:
		r.cat, r.pose = new(d.cat), pose
	}
	if r.cat == nil && r.app == nil && (mood == "thinking" || mood == "speaking") {
		r.view.actions = maps.Clone(r.view.actions)
		r.view.stopControl()
	}
	d.renderGen++
	d.renderNext = &r
	if !d.rendering {
		d.rendering = true
		go h.renderFrames(d)
	}
}

// A device has one frame builder and one replaceable pending request. A dirty
// rectangle is always based on the last frame committed to that device's queue.
func (h *wsHub) renderFrames(d *wsDevice) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for d.renderNext != nil && h.active(d) {
		r, gen, prev, rev := *d.renderNext, d.renderGen, d.svg, d.rev+1
		web := d.client.role == "web"
		var geometry screenGeometry
		if d.client.role == "device" {
			geometry = d.screen
		}
		d.renderNext = nil
		h.mu.Unlock()

		screen := r.screen
		var err error
		if r.app == nil {
			var face string
			if r.cat != nil {
				r.view = catChrome(*r.cat, r.pose)
				face = catArt(*r.cat, r.pose)
			}
			raw := r.view.svg(face)
			if !r.full && raw == prev.raw {
				screen = prev
			} else {
				screen, err = h.b.buildScreen(raw, false, geometry)
			}
		}
		var svgWire, native wsFrame
		var rawBytes int
		if err == nil && (r.full && r.app == nil || screen.source != prev.source) {
			start := time.Now()
			if screen.fb != nil {
				rect := dirtyRect(prev.fb, screen.fb, geometry)
				if !rect.Empty() {
					native = displayFrame(screen.fb, rect, rev, geometry.width)
					rawBytes = 12 + rect.Dx()*rect.Dy()*2
				}
			}
			screen.render += time.Since(start)
			svgWire = textFrame(object{"op": "svg", "rev": rev, "svg": screen.source})
		}

		h.mu.Lock()
		if !h.active(d) || d.renderGen != gen {
			continue
		}
		if err != nil {
			log.Printf("EchoEar screen: %v", err)
			continue
		}
		d.scene, d.displayApp, d.svg = r.view, r.app, screen
		if svgWire.data == nil {
			continue
		}
		d.rev, d.svgWire = rev, svgWire
		if web {
			d.client.enqueue(svgWire)
			h.trackFrame(d, screen.render, len(svgWire.data), len(screen.source), "svg")
		} else if native.data != nil {
			d.client.enqueue(native)
			codec := "RGB565"
			if native.data[9] == 1 {
				codec = "RLE565"
			}
			h.trackFrame(d, screen.render, len(native.data), rawBytes, codec)
		}
		for c := range h.clients {
			if c.watch == d.key {
				c.enqueue(svgWire)
			}
		}
		if d.announcing {
			h.announce(d)
		}
	}
	d.rendering, d.renderNext = false, nil
}

func (h *wsHub) wake(d *wsDevice, now time.Time) {
	d.last = now
	h.setSleeping(d, false)
}

func (h *wsHub) setSleeping(d *wsDevice, asleep bool) {
	if d.asleep != asleep {
		d.asleep = asleep
		h.push(d, object{"op": "power", "brightness": d.brightness()}, true)
	}
}

func (d *wsDevice) brightness() int {
	if d.asleep {
		return 12
	}
	return 85
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
			h.tickApps(now)
			for _, d := range h.devices {
				switch {
				case !d.recAt.IsZero() && d.recStopTimer == nil && now.Sub(d.recAt) > 90*time.Second:
					h.stopRecordingAtLimit(d, now)
				case d.awaitAck && !d.ttsStarted && now.After(d.audioUntil.Add(4*time.Second)):
					h.playbackDone(d, now)
				case !d.asleep && !d.busy && d.recAt.IsZero() && !d.ttsStarted && !d.awaitAck && now.Sub(d.last) >= idleSleep:
					h.setSleeping(d, true)
					h.render(d, now, false)
				case d.drawer == "stats" && !d.asleep && now.Sub(d.statsAt) >= diagInterval:
					h.render(d, now, false)
				case (d.drawer == "" || d.asleep) && (d.app == nil && d.view == nil || d.asleep || !d.recAt.IsZero()):
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

func (d *wsDevice) volumeFrame(persist bool) object {
	return object{"op": "volume", "device_id": d.who.DeviceID, "level": d.vol, "auto_volume": d.autoVol, "persist": persist}
}

func (h *wsHub) saveVoice(id, voice string) error {
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	if err := h.b.voices.save(id, voice); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.devices[strings.ToLower(id)]; d != nil && h.active(d) {
		d.voice = voice
		if d.drawer != "" {
			h.render(d, time.Now(), false)
		}
	}
	return nil
}

// Takes and returns with h.mu held. Serialize writes without blocking the hub.
func (h *wsHub) setVolume(ctx context.Context, d *wsDevice, auto bool, level int, action string) error {
	h.mu.Unlock()
	h.settingsMu.Lock()
	defer h.settingsMu.Unlock()
	h.mu.Lock()
	if !h.active(d) {
		return problem(409, "Device is not connected")
	}
	switch action {
	case "vol-minus":
		level = max(0, d.vol-10)
	case "vol-plus":
		level = min(100, d.vol+10)
	case "vol-auto":
		auto, level = !d.autoVol, d.vol
	}
	if level < 0 {
		level = d.vol
	}
	savedLevel := level
	if auto {
		savedLevel = initialAutoVolume
		if !d.autoVol {
			level = initialAutoVolume
		} else {
			level = d.vol
		}
	}
	h.mu.Unlock()
	err := h.b.saveVolumePrefs(ctx, d.key, auto, savedLevel)
	h.mu.Lock()
	if err != nil {
		return err
	}
	if !h.active(d) {
		return problem(409, "Device disconnected while saving volume")
	}
	if auto != d.autoVol {
		d.hasAmbient = false
	}
	changedMode := d.autoVol != auto
	d.autoVol, d.vol = auto, level
	h.push(d, d.volumeFrame(!auto), true)
	now := time.Now()
	d.meter = now.Add(1800 * time.Millisecond)
	h.render(d, now, false)
	if changedMode {
		h.broadcastPeers()
	}
	return nil
}
