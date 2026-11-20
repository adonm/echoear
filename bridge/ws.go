package main

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	Proto     int    `json:"proto"`
	Port      *int   `json:"port"`
	Width     *int   `json:"width"`
	Height    *int   `json:"height"`
	Shape     string `json:"shape"`
	Board     string `json:"board"`
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

type wsClient struct {
	wire       *websocket
	ctx        context.Context
	cancel     context.CancelFunc
	out        chan wsFrame
	outMu      sync.Mutex
	drained    chan struct{}
	role       string
	watch      string
	device     *wsDevice
	scope      *virtualClient
	pingMu     sync.Mutex
	pingSentAt time.Time
	rtt        *float64
}

type wsDevice struct {
	client     *wsClient
	who        peer
	key        string
	firmware   string
	screen     screenGeometry
	board      string
	theme      string
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
	svgRaw     string
	app        *appInstance
	appView    svgScreen

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

// The hub lock orders state changes and control frames. Socket writes,
// playback queue waits, and backend requests never run under it.
type wsHub struct {
	b       *bridge
	mu      sync.Mutex
	conns   sync.WaitGroup
	clients map[*wsClient]struct{}
	devices map[string]*wsDevice
	closed  bool
	peers   string
}

func newWSHub(b *bridge) *wsHub {
	h := &wsHub{b: b, clients: make(map[*wsClient]struct{}), devices: make(map[string]*wsDevice)}
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
	wire.onPong = c.pong
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
	ping := time.NewTicker(12 * time.Second)
	defer ping.Stop()
	for c.ctx.Err() == nil {
		select {
		case <-c.ctx.Done():
		case f := <-c.out:
			c.outMu.Lock()
			if c.drained != nil {
				close(c.drained)
				c.drained = nil
			}
			c.outMu.Unlock()
			if c.wire.write(f) != nil {
				return
			}
		case <-ping.C:
			if c.wire.write(c.ping(time.Now())) != nil {
				return
			}
		}
	}
	_ = c.wire.write(wsCloseFrame(1001))
}

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

func (c *wsClient) enqueue(f wsFrame) bool {
	c.outMu.Lock()
	defer c.outMu.Unlock()
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

func (c *wsClient) enqueueWait(ctx context.Context, f wsFrame) bool {
	for {
		c.outMu.Lock()
		if c.ctx.Err() != nil || ctx.Err() != nil {
			c.outMu.Unlock()
			return false
		}
		// Keep one FIFO, but leave room for display and interrupt controls
		// rather than letting a burst of PCM fill it.
		if len(c.out) < min(8, cap(c.out)) {
			c.out <- f
			c.outMu.Unlock()
			return true
		}
		if c.drained == nil {
			c.drained = make(chan struct{})
		}
		drained := c.drained
		c.outMu.Unlock()
		select {
		case <-drained:
		case <-c.ctx.Done():
			return false
		case <-ctx.Done():
			return false
		}
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
		h.dropApp(d)
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
		h.dropApp(d)
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
		list, err := b.peers.snapshot(sender)
		for i := range list {
			list[i].Character = b.catFor(list[i]).Name
		}
		return list, err
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
		stored[i].Character = h.b.catFor(p).Name
	}
	for _, d := range h.devices {
		if !h.active(d) || strings.EqualFold(sender, d.who.DeviceID) {
			continue
		}
		p := d.who
		p.Character = d.cat.Name
		if d.virtual() {
			p.IP = ""
		}
		if i, ok := index[d.key]; ok {
			stored[i] = p
		} else {
			stored = append(stored, p)
		}
	}
	slices.SortFunc(stored, func(a, b peer) int { return strings.Compare(a.DeviceID, b.DeviceID) })
	return stored, nil
}

func (h *wsHub) peerFrame() wsFrame {
	list, _ := h.combinedPeers("")
	p := publicPeers(list)
	for i, entry := range p {
		if d := h.devices[strings.ToLower(list[i].DeviceID)]; d != nil && h.active(d) {
			entry["ws"], entry["online"], entry["firmware"] = true, true, d.firmware
			entry["cat"] = d.cat.catLook
			entry["auto_volume"] = d.autoVol
		} else {
			entry["ws"] = false
			entry["cat"] = h.b.catFor(list[i]).catLook
			entry["auto_volume"], _ = h.b.volumePrefs(list[i].DeviceID)
		}
	}
	return textFrame(object{"op": "peers", "devices": p})
}

func (h *wsHub) scopedPeerFrame() wsFrame {
	list, _ := h.combinedPeers("")
	devices := make([]object, 0, len(list))
	for _, p := range list {
		if d := h.devices[strings.ToLower(p.DeviceID)]; d != nil && h.active(d) {
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
		c.role = "web"
		c.enqueue(h.peerFrame())
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
		theme: v.Theme, pct: -1, last: now, moodAt: now}
	d.cat = h.b.catFor(d.who)
	d.autoVol, d.vol = h.b.volumePrefs(d.key)
	if ev.Role == "device" {
		d.telemetry.HasBattery = new(board == "esp-vocat")
	}
	d.status(ev.wsStatus)
	if old := h.devices[d.key]; old != nil {
		h.dropApp(old)
		h.cancelTurn(old)
		h.stopPlayback(old, true)
		old.client.cancel()
		d.rev = old.rev
	}
	h.devices[d.key], c.device, c.role = d, d, ev.Role
	h.render(d, now, true)
	if c.role == "web" {
		c.enqueue(catFrame(d.who.DeviceID, d.cat, h.b.voices.get(d.who.DeviceID, d.voiceTheme())))
	}
	h.push(d, d.volumeFrame(false), true)
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
		name := d.cat.Name
		go func() {
			ctx, cancel := context.WithTimeout(c.ctx, ttsWait)
			defer cancel()
			h.b.warmCharacter(ctx, h.b.voices.get(d.who.DeviceID, d.voiceTheme()), name)
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
	var view sceneView
	var svg string
	switch {
	case d.drawer != "" && mood == "idle":
		switch d.drawer {
		case "volume":
			view = volumeScene(d.autoVol, d.vol)
		case "voice":
			view = voiceScene(h.b.voices.get(d.who.DeviceID, d.voiceTheme()))
		case "intercom":
			peers, _ := h.combinedPeers(d.who.DeviceID)
			choices := []menuPeer{{"all", "Everyone", "#FFADAC", "#FFD68B"}}
			for _, p := range peers {
				if device := h.devices[strings.ToLower(p.DeviceID)]; device != nil && h.active(device) {
					choices = append(choices, menuPeer{p.DeviceID, p.Character, device.cat.FaceBG, device.cat.AccentColor})
				}
			}
			d.peerPage = min(max(0, d.peerPage), (len(choices)-1)/4)
			view = intercomSceneMenu(choices, d.peerPage)
		case "stats":
			if d.client.scope == nil {
				d.statsAt = now
				view = h.statsScene(d)
				break
			}
			fallthrough
		default:
			view = menuHome(h.b.voices.get(d.who.DeviceID, d.voiceTheme()), d.autoVol, d.vol, d.client.scope != nil)
		}
	case mood == "listening":
		view = catChrome(d.cat, pose)
		svg = view.svg(catArt(d.cat, pose))
	case d.card != nil && (mood == "idle" || mood == "speaking" || mood != "sleepy" && d.card.duringTurn):
		view = *d.card
	case d.app != nil && mood != "sleepy":
		d.svgRaw = ""
		d.scene = sceneView{title: d.app.spec.Title}
		h.pushScreen(d, d.appView, full)
		return
	case d.view != nil && mood != "sleepy":
		view = *d.view
	default:
		view = catChrome(d.cat, pose)
		svg = view.svg(catArt(d.cat, pose))
	}
	if svg == "" {
		if mood == "thinking" || mood == "speaking" {
			view.actions = maps.Clone(view.actions)
			view.stopControl()
		}
		svg = view.svg("")
	}
	d.scene = view
	h.renderSVGView(d, svg, full)
}

func (h *wsHub) wake(d *wsDevice, now time.Time) {
	d.last = now
	h.setSleeping(d, false)
}

func (h *wsHub) setSleeping(d *wsDevice, asleep bool) {
	if d.asleep != asleep {
		d.asleep = asleep
		brightness := 85
		if asleep {
			brightness = 12
		}
		h.push(d, object{"op": "power", "brightness": brightness}, true)
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
			h.tickApps(now)
			for _, d := range h.devices {
				switch {
				case !d.recAt.IsZero() && now.Sub(d.recAt) > 90*time.Second:
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
				c.enqueue(h.svgFrame(d))
				c.enqueue(textFrame(d.volumeFrame(false)))
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
		if ev.Event != "tap" && ev.Event != "text" && ev.Event != "head" {
			return
		}
	}
	if d == nil || !h.active(d) {
		return
	}
	if c.scope != nil && ev.Event == "watch" && ev.DeviceID == c.scope.ID {
		c.enqueue(h.svgFrame(d))
		return
	}
	switch ev.Event {
	case "status":
		d.status(ev.wsStatus)
		if d.drawer != "stats" {
			h.render(d, now, false)
		}
	case "tap":
		h.pointer(d, ev, now, c == d.client && c.role == "device")
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
	if d.app != nil && d.svg.source == d.appView.source {
		if i := slices.Index(d.app.devices, d); i >= 0 {
			h.updateApp(d.app, i, key, ev.X, ev.Y, now)
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
		playing := d.ttsStarted || d.awaitAck
		h.leaveScene(d, now)
		if !playing {
			h.chime(d, "cancel")
		}
	case "cat":
		h.leaveScene(d, now)
	case "dismiss":
		d.card = nil
		h.render(d, now, false)
	case "voice-select":
		if err := h.b.voices.save(d.who.DeviceID, a.value); err == nil {
			h.chime(d, "tap")
			h.render(d, now, false)
		}
	case "vol-auto", "vol-minus", "vol-plus":
		level := d.vol
		switch a.kind {
		case "vol-minus":
			level = max(0, level-10)
		case "vol-plus":
			level = min(100, level+10)
		}
		if err := h.setVolume(d.client.ctx, d, a.kind == "vol-auto" && !d.autoVol, level, now); err != nil {
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
	h.dropApp(d)
	d.drawer, d.view, d.pending, d.card = "", nil, nil, nil
}

func (d *wsDevice) volumeFrame(persist bool) object {
	return object{"op": "volume", "device_id": d.who.DeviceID, "level": d.vol, "auto_volume": d.autoVol, "persist": persist}
}

func (h *wsHub) setVolume(ctx context.Context, d *wsDevice, auto bool, level int, now time.Time) error {
	savedLevel := level
	if auto {
		savedLevel = initialAutoVolume
		if !d.autoVol {
			level = initialAutoVolume
		} else {
			level = d.vol
		}
	}
	if err := h.b.saveVolumePrefs(ctx, d.key, auto, savedLevel); err != nil {
		return err
	}
	if auto != d.autoVol {
		d.hasAmbient = false
	}
	changedMode := d.autoVol != auto
	d.autoVol, d.vol = auto, level
	h.push(d, d.volumeFrame(!auto), true)
	d.meter = now.Add(1800 * time.Millisecond)
	h.render(d, now, false)
	if changedMode {
		h.broadcastPeers()
	}
	return nil
}
