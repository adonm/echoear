package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type bridge struct {
	db        *bridgeDB
	root      context.Context
	token     string
	peers     peers
	hex       hexClient
	oc        *openCode
	pairPort  int
	worker    *synthWorker
	voices    deviceVoices
	clips     speechClips
	audio     audioCache
	chime     []byte
	peerHTTP  *http.Client
	ota       otaFile
	box3OTA   otaFile
	ws        *wsHub
	virtual   *store[virtualClient]
	tunnels   *tunnelManager
	history   *chatHistory
	timers    *timerManager
	catTitles sync.Mutex
}

func sendBytes(w http.ResponseWriter, status int, kind string, data []byte) {
	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func sendJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		data, status = []byte(`{"error":"Cannot encode response"}`), 500
	}
	sendBytes(w, status, jsonType, data)
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	w.Header().Set("Cache-Control", "no-store")
	if path == "/mcp" {
		b.serveMCP(w, r)
		return
	}
	if strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/ota/") {
		if !tokenMatches(r.Header.Values("X-EchoEar-Token"), b.token) {
			sendError(w, problem(401, "Unauthorized"))
			return
		}
	}
	value, err := b.route(w, r, path)
	sendResult(w, value, err)
}

func sendResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		sendError(w, err)
	} else if value != nil {
		sendJSON(w, 200, value)
	}
}

func sendError(w http.ResponseWriter, err error) {
	status := 502
	var api *apiError
	var upstream *upstreamStatus
	if errors.As(err, &api) {
		status = api.status
	} else if errors.As(err, &upstream) {
		status = upstream.status
	}
	sendJSON(w, status, object{"error": err.Error()})
}

func body(r *http.Request, audio bool) ([]byte, string, error) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if audio && kind != pcmType && kind != wavType {
		return nil, "", problem(415, "Expected audio/pcm or audio/wav")
	}
	if !audio && kind != jsonType {
		return nil, "", problem(415, "Expected application/json")
	}
	lengths := r.Header.Values("Content-Length")
	if len(r.TransferEncoding) != 0 || len(lengths) > 1 {
		return nil, "", problem(400, "Request requires a single Content-Length")
	}
	if len(lengths) == 0 && r.ContentLength <= 0 {
		return nil, "", problem(411, "Content-Length required")
	}
	if len(lengths) != 0 && len(lengths[0]) > 12 {
		return nil, "", problem(400, "Invalid Content-Length")
	}
	limit := int64(maxJSON)
	if audio {
		limit = maxPCM
	}
	if r.ContentLength > limit {
		return nil, "", problem(413, "Request body too large")
	}
	if r.ContentLength < 0 {
		return nil, "", problem(400, "Invalid Content-Length")
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		var timed net.Error
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, "", problem(408, "Request body timed out")
		}
		return nil, "", problem(400, "Incomplete request body")
	}
	if kind == pcmType && (len(buf) == 0 || len(buf)%2 != 0) {
		return nil, "", problem(400, "PCM must contain whole 16-bit samples")
	}
	if kind == wavType && (len(buf) < 12 || string(buf[:4]) != "RIFF" || string(buf[8:12]) != "WAVE") {
		return nil, "", problem(400, "Expected RIFF/WAVE audio")
	}
	return buf, kind, nil
}

func jsonBody(r *http.Request) (object, error) {
	raw, _, err := body(r, false)
	if err != nil {
		return nil, err
	}
	return decodeObject(raw)
}

func deviceID(r *http.Request, obj object, required bool) (string, error) {
	v, provided := obj["device_id"]
	if !provided && !required {
		v = "echoear"
		if values := r.Header.Values("X-Device-Id"); len(values) > 0 {
			v = values[0]
		}
	}
	id := str(v)
	if !deviceIDRE.MatchString(id) {
		return "", problem(400, "Invalid device ID")
	}
	return id, nil
}

func (b *bridge) voiceFor(who peer) string {
	return b.voices.get(who.DeviceID, b.voiceTheme(who))
}

func (b *bridge) voiceTheme(who peer) string {
	theme := deviceTheme(who.Character)
	if v, ok := b.virtualByID(strings.ToLower(who.DeviceID)); ok {
		return v.Theme
	}
	if b.peers.store != nil {
		if p, ok, _ := b.peers.get(who.DeviceID); ok {
			return deviceTheme(p.Character)
		}
	}
	return theme
}

func voiceDevice(path string) (string, bool) {
	suffix, ok := strings.CutPrefix(path, "/api/devices/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(suffix, "/voice")
	return id, ok && deviceIDRE.MatchString(id)
}

func (b *bridge) voiceAPI(r *http.Request, id string) (any, error) {
	obj, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	voice := str(obj["voice"])
	if !b.voices.allowed(voice) {
		return nil, problem(400, "Unsupported voice")
	}
	if err := b.voices.save(id, voice); err != nil {
		return nil, err
	}
	if b.ws != nil {
		b.ws.mu.Lock()
		if d := b.ws.devices[strings.ToLower(id)]; d != nil && d.drawer {
			b.ws.render(d, time.Now(), false)
		}
		b.ws.mu.Unlock()
	}
	return object{"device_id": id, "voice": voice}, nil
}

func (b *bridge) warmCharacter(ctx context.Context, voice, character string) {
	for _, intro := range []string{"From ", "Message from ", "Sent to "} {
		if _, err := b.clips.get(ctx, b.worker, voice, intro+character+"!"); err != nil {
			return
		}
	}
}

func (b *bridge) identity(r *http.Request, obj object, register bool) (peer, error) {
	device, err := deviceID(r, obj, register)
	if err != nil {
		return peer{}, err
	}
	if virtualID.MatchString(strings.ToLower(device)) {
		return peer{}, problem(400, "Virtual clients use /api/chat and WebSocket")
	}
	source, _, _ := net.SplitHostPort(r.RemoteAddr)
	if addr, err := netip.ParseAddr(source); err == nil {
		source = addr.Unmap().String()
	}
	sourceIP, _ := netip.ParseAddr(source)
	character := ""
	if register {
		character = strings.TrimSpace(str(obj["character"]))
	} else if chars := r.Header.Values("X-Character"); len(chars) > 0 {
		character = strings.TrimSpace(chars[0])
	}
	if (register || len(r.Header.Values("X-Character")) > 0) && !characterRE.MatchString(character) {
		return peer{}, problem(400, "Invalid character name")
	}
	ip := source
	if v, ok := obj["ip"]; ok && register {
		ip = str(v)
	}
	ip, err = lanIP(ip, sourceIP.IsLoopback())
	if err != nil {
		return peer{}, err
	}
	if !register {
		who, err := b.peers.touch(device, ip, character)
		if err == nil {
			who.Character = b.catFor(who).Name
		}
		return who, err
	}
	port := int64(80)
	if v, exists := obj["port"]; exists {
		var ok bool
		var n json.Number
		n, ok = v.(json.Number)
		if !ok {
			return peer{}, problem(400, "Invalid peer port")
		}
		port, err = n.Int64()
		if err != nil || port < 1 || port > 65535 {
			return peer{}, problem(400, "Invalid peer port")
		}
	}
	p := peer{device, character, ip, int(port), time.Now().Unix()}
	if err := b.peers.register(p); err != nil {
		return peer{}, err
	}
	if b.ws != nil {
		b.ws.peersChanged()
	}
	if b.worker != nil {
		go func() {
			ctx, cancel := context.WithTimeout(b.root, ttsWait)
			defer cancel()
			b.warmCharacter(ctx, b.voiceFor(p), character)
		}()
	}
	return p, nil
}

type answer struct {
	Reply          string    `json:"reply"`
	SessionID      string    `json:"session_id"`
	Screen         object    `json:"screen"`
	OpenCodeMS     int64     `json:"opencode_ms"`
	AudioURL       *string   `json:"audio_url"`
	AudioBytes     int       `json:"audio_bytes"`
	SampleRate     int       `json:"sample_rate"`
	AudioStreaming bool      `json:"audio_streaming"`
	TTSFirstMS     int64     `json:"tts_first_ms"`
	TTSMS          int64     `json:"tts_ms"`
	OK             *bool     `json:"ok,omitempty"`
	SentTo         *[]string `json:"sent_to,omitempty"`
	DeliveredTo    *[]string `json:"delivered_to,omitempty"`
	FailedTo       *[]string `json:"failed_to,omitempty"`
	Error          string    `json:"error,omitempty"`
}

type voiceAnswer struct {
	Transcript string `json:"transcript"`
	HexMS      int64  `json:"hex_ms"`
	answer
}

func (b *bridge) attachAudio(ctx context.Context, out *answer, stream *AudioStream, start time.Time) {
	if stream != nil {
		if pcm, err := stream.chunk(ctx, 0); err == nil && len(pcm) != 0 {
			if id, size, ongoing := b.audio.add(stream); id != "" {
				path := "/api/audio/" + id
				out.AudioURL, out.AudioBytes, out.AudioStreaming = &path, size, ongoing
			}
		}
	}
	out.TTSFirstMS = time.Since(start).Milliseconds()
	out.TTSMS = out.TTSFirstMS
}

type delivery struct {
	sent, delivered, failed []string
	err                     string
}

func (b *bridge) postPeer(ctx context.Context, who peer, path, kind string, body ...[]byte) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	readers := make([]io.Reader, len(body))
	var size int64
	for i, chunk := range body {
		readers[i] = bytes.NewReader(chunk)
		size += int64(len(chunk))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://"+net.JoinHostPort(who.IP, strconv.Itoa(who.Port))+path, io.MultiReader(readers...))
	if err != nil {
		return false
	}
	req.ContentLength = size
	req.Header.Set("X-EchoEar-Token", b.token)
	req.Header.Set("Content-Type", kind)
	res, err := b.peerHTTP.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 16<<10))
	return res.StatusCode >= 200 && res.StatusCode < 300
}

func (b *bridge) deliverHTTP(ctx context.Context, sender peer, recipients []peer, voice, text string, raw []byte) delivery {
	out := delivery{sent: []string{}, delivered: []string{}, failed: []string{}}
	if len(recipients) == 0 {
		out.err = "peer_not_found"
		return out
	}
	fail := func(reason string) delivery {
		out.err = reason
		for _, p := range recipients {
			out.failed = append(out.failed, p.DeviceID)
		}
		return out
	}
	if b.worker == nil || !b.worker.ready.Load() {
		return fail("audio_unavailable")
	}
	text = shortText(text, maxMessage, true)
	label := "Message from "
	if raw != nil {
		label = "From "
	}
	intro, err := b.clips.get(ctx, b.worker, voice, label+sender.Character+"!")
	if err != nil {
		return fail("audio_unavailable")
	}
	var message []byte
	if raw == nil {
		clean, err := spokenText(text)
		if err != nil {
			return fail("audio_unavailable")
		}
		message, err = b.worker.synthesize(ctx, voice, clean)
		if err != nil {
			return fail("audio_unavailable")
		}
	} else {
		message = NormalizeVoicePCM(raw)
	}
	if len(b.chime)+len(intro)+len(message) > maxPeerPCM {
		return fail("message_too_long")
	}
	display, _ := json.Marshal(messageCard(sender, text))
	ok := make([]bool, len(recipients))
	var wg sync.WaitGroup
	bound := make(chan struct{}, 8)
	for i, who := range recipients {
		select {
		case bound <- struct{}{}:
		case <-ctx.Done():
			break
		}
		if ctx.Err() != nil {
			break
		}
		wg.Go(func() {
			defer func() { <-bound }()
			displayOK := make(chan bool, 1)
			go func() { displayOK <- b.postPeer(ctx, who, "/display", jsonType, display) }()
			speakOK := b.postPeer(ctx, who, "/speak", pcmType, b.chime, intro, message)
			ok[i] = <-displayOK && speakOK
		})
	}
	wg.Wait()
	for i, who := range recipients {
		if ok[i] {
			out.sent = append(out.sent, who.DeviceID)
			out.delivered = append(out.delivered, who.Character)
		} else {
			out.failed = append(out.failed, who.DeviceID)
		}
	}
	if len(out.sent) == 0 {
		out.err = "delivery_failed"
	}
	return out
}

func (b *bridge) relay(ctx context.Context, sender peer, others []peer, target, text string, pcm []byte) (answer, error) {
	recipients := targets(others, target)
	d := b.deliver(ctx, sender, recipients, text, pcm)
	recipient := target
	if broadcast(target) {
		recipient = "everyone"
	} else if len(recipients) > 0 {
		recipient = recipients[0].Character
	}
	ok := len(d.delivered) > 0
	reply := "Sent to " + recipient + "!"
	if !ok {
		reply = "I couldn't reach " + recipient + " right now."
	} else if len(d.failed) != 0 {
		reply = "I sent your message, but couldn't reach everyone."
	}
	sid, err := b.oc.get(sender.DeviceID)
	if err != nil {
		return answer{}, err
	}
	out := answer{Reply: reply, SessionID: sid, SampleRate: sampleRate, OK: &ok,
		SentTo: &d.sent, DeliveredTo: &d.delivered, FailedTo: &d.failed, Error: d.err}
	start := time.Now()
	if b.worker != nil && b.worker.ready.Load() {
		if pcm, err := b.clips.get(ctx, b.worker, b.voiceFor(sender), reply); err == nil {
			s := newAudioStream()
			s.append(pcm)
			s.finish(nil)
			b.attachAudio(ctx, &out, s, start)
		}
	}
	out.TTSFirstMS = time.Since(start).Milliseconds()
	out.TTSMS = out.TTSFirstMS
	return out, nil
}

func (b *bridge) turn(ctx context.Context, sender peer, text string, body []byte, kind string) (answer, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return answer{}, problem(400, "Expected nonempty text")
	}
	others, err := b.peerSnapshot(sender.DeviceID)
	if err != nil {
		return answer{}, err
	}
	source := "typed"
	if body != nil {
		source = "voice"
	}
	if direct := directMessage(others, text); direct != nil {
		b.chat(sender.DeviceID, newChat("user", "You", text, source), true)
		pcm := body
		if kind == wavType {
			pcm, err = wavPCM(body)
			if err != nil {
				return answer{}, err
			}
		}
		return b.relay(ctx, sender, others, direct.target, direct.text, pcm)
	}
	options := turnOptions{source: source}
	gen, err := b.generateReply(ctx, b.root, sender, text, options)
	if err != nil {
		return answer{}, err
	}
	sent := []string{}
	out := answer{Reply: gen.reply, SessionID: gen.sid, Screen: gen.screen, OpenCodeMS: gen.openMS, SampleRate: sampleRate, SentTo: &sent}
	if gen.reply != "" {
		b.attachAudio(ctx, &out, gen.audio, gen.ttsStart)
	}
	if gen.message != nil {
		d := b.deliver(ctx, sender, targets(others, gen.message.target), gen.message.text, nil)
		out.SentTo, out.DeliveredTo, out.FailedTo, out.Error = &d.sent, &d.delivered, &d.failed, d.err
	}
	return out, nil
}

type generatedReply struct {
	reply, sid string
	screen     object
	message    *intercomMessage
	audio      *AudioStream
	openMS     int64
	ttsStart   time.Time
}

type turnOptions struct {
	source  string
	web     bool
	publish func(*AudioStream)
}

type turnContext struct {
	at                         time.Time
	screen, room, lastIntercom string
	timers                     int
}

func turnPrompt(sender peer, web bool, source, text string, cat catState, _ turnContext) string {
	kind := "hardware"
	if web || webDeviceID(strings.ToLower(sender.DeviceID)) {
		kind = "web"
	}
	extra := ""
	if !cat.Configured {
		extra += " · setup"
	}
	if cat.Personality != "" {
		extra += " · vibe: " + cat.Personality
	}
	return fmt.Sprintf("[%s (%s) · %s · %s%s]\n%s", cat.Name, sender.DeviceID, kind, source, extra, text)
}

// SSE overlaps synthesis with the turn. Only the verified final reply is audible.
func (b *bridge) generateReply(ctx, audioBase context.Context, sender peer, text string, options turnOptions) (generatedReply, error) {
	makeSpeech := func() *speechTurn {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadline = time.Now().Add(turnTimeout)
		}
		audioCtx, cancel := context.WithDeadline(audioBase, deadline)
		return newSpeechTurn(audioCtx, cancel, b.worker, b.voiceFor(sender))
	}
	var eager *speechTurn
	onSentence := func(s string) {
		if b.worker == nil || !b.worker.ready.Load() {
			return
		}
		if eager == nil {
			eager = makeSpeech()
		}
		eager.send(s)
	}
	start := time.Now()
	prepare := func() (string, string) {
		cat := b.catFor(sender)
		return cat.Name, turnPrompt(sender, options.web, options.source, text, cat, turnContext{})
	}
	raw, sid, err := b.oc.askPrepared(ctx, sender.DeviceID, prepare, onSentence, options.source, nil, func(entry chatEntry) {
		if entry.Role == "assistant" {
			entry.Sender = b.catFor(sender).Name
		}
		b.chat(sender.DeviceID, entry, true)
	})
	openMS := time.Since(start).Milliseconds()
	if err != nil {
		if eager != nil {
			eager.discard()
		}
		return generatedReply{}, err
	}
	reply, screen, message := extractPayloads(raw)
	ttsStart := time.Now()
	var stream *AudioStream
	clean, speechErr := spokenText(fenced.ReplaceAllString(reply, ""))
	if speechErr == nil && (eager != nil || b.worker != nil && b.worker.ready.Load()) {
		final := SplitSentences(clean)
		if eager == nil {
			eager = makeSpeech()
		}
		if !eager.complete(final) {
			eager.discard()
			eager = makeSpeech()
			eager.complete(final)
		}
		stream = eager.stream
		if options.publish != nil {
			options.publish(stream)
		}
	} else if eager != nil {
		eager.discard()
	}
	return generatedReply{reply, sid, screen, message, stream, openMS, ttsStart}, nil
}

func (b *bridge) voice(ctx context.Context, r *http.Request, intercom bool) (any, error) {
	target := strings.TrimSpace(r.Header.Get("X-Target"))
	if intercom && (target == "" || runeLen(target) > 64) {
		return nil, problem(400, "Expected X-Target peer name")
	}
	wire, kind, err := body(r, true)
	if err != nil {
		return nil, err
	}
	pcm := wire
	if intercom && kind == wavType {
		pcm, err = wavPCM(wire)
		if err != nil {
			return nil, err
		}
	}
	sender, err := b.identity(r, nil, false)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	transcript, err := b.hex.transcribe(ctx, wire, kind)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return nil, err
	}
	if transcript != "" {
		b.deviceEvent(sender.DeviceID, "speech", object{"text": transcript, "source": "voice"})
	}
	out := answer{SampleRate: sampleRate}
	switch {
	case intercom:
		others, err := b.peerSnapshot(sender.DeviceID)
		if err != nil {
			return nil, err
		}
		out, err = b.relay(ctx, sender, others, target, transcript, pcm)
		if err != nil {
			return nil, err
		}
	case transcript != "":
		out, err = b.turn(ctx, sender, transcript, wire, kind)
	default:
		out.SessionID, err = b.oc.get(sender.DeviceID)
	}
	if err != nil {
		return nil, err
	}
	return voiceAnswer{transcript, ms, out}, nil
}

func (b *bridge) route(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/cats" || strings.HasPrefix(path, "/api/cats/") ||
		strings.HasPrefix(path, "/api/devices/") && strings.HasSuffix(path, "/cat"):
		return b.catAPI(w, r, path)
	case path == "/api/memory" || path == "/api/memory/scopes":
		return b.memoryAPI(w, r, path)
	case path == "/api/assets" || strings.HasPrefix(path, "/api/assets/"):
		return b.assetAPI(w, r, path)
	case path == "/api/apps" || strings.HasPrefix(path, "/api/apps/"):
		return b.appAPI(w, r, path)
	case path == "/api/timers" || strings.HasPrefix(path, "/api/timers/"):
		return b.timerAPI(w, r, path, "")
	case path == "/api/virtual-clients" || strings.HasPrefix(path, "/api/virtual-clients/"):
		return b.virtualAPI(w, r, path)
	case path == "/api/tunnel/status" || path == "/api/tunnels" || strings.HasPrefix(path, "/api/tunnels/"):
		return b.tunnelAPI(w, r, path)
	case r.Method == http.MethodPost && (path == "/api/chat" || path == "/api/reset"):
		return b.chatAPI(r, path, "")
	}
	switch r.Method {
	case http.MethodGet:
		switch {
		case path == "/":
			http.Redirect(w, r, "/app", http.StatusFound)
			return nil, nil
		case serveApp(w, path):
			return nil, nil
		case path == "/ws" && b.ws != nil:
			return nil, b.ws.serve(w, r)
		case path == "/health":
			h := make(chan bool, 1)
			go func() { h <- b.hex.reachable(ctx, "/health") }()
			o := b.oc.reachable(ctx, "/api/info")
			ready := b.worker != nil && b.worker.ready.Load()
			engine := "unavailable"
			if ready {
				engine = "kokoro-onnx"
			}
			return object{"ok": true, "hex": <-h, "opencode": o, "tts": ready, "tts_engine": engine}, nil
		case path == "/api/devices":
			devices, err := b.deviceList()
			return object{"devices": devices}, err
		case path == "/api/scope":
			return object{"scoped": false}, nil
		case strings.HasSuffix(path, "/messages"):
			if id, ok := messagesDevice(path); ok {
				return b.messages(ctx, id)
			}
		case strings.HasPrefix(path, "/api/audio/"):
			return nil, b.audio.ServeAudio(w, r, strings.TrimPrefix(path, "/api/audio/"))
		case path == "/ota/version.json" || path == "/ota/latest.bin":
			return b.ota.serve(w, path == "/ota/version.json")
		case path == "/ota/box3/version.json" || path == "/ota/box3/latest.bin":
			return b.box3OTA.serve(w, path == "/ota/box3/version.json")
		}
	case http.MethodPost:
		if path == "/api/opencode/pair" {
			return b.pairOpenCode(r)
		}
		if id, ok := voiceDevice(path); ok {
			return b.voiceAPI(r, id)
		}
		ctx, cancel := context.WithTimeout(ctx, turnTimeout)
		defer cancel()
		if path == "/api/voice" || path == "/api/intercom" {
			return b.voice(ctx, r, path == "/api/intercom")
		}
		switch path {
		case "/api/register", "/api/text", "/api/tts":
			obj, err := jsonBody(r)
			if err != nil {
				return nil, err
			}
			switch path {
			case "/api/register":
				sender, err := b.identity(r, obj, true)
				if err != nil {
					return nil, err
				}
				others, err := b.peerSnapshot(sender.DeviceID)
				return object{"ok": true, "peers": publicPeers(others)}, err
			case "/api/text":
				sender, err := b.identity(r, obj, false)
				if err != nil {
					return nil, err
				}
				return b.turn(ctx, sender, str(obj["text"]), nil, "")
			case "/api/tts":
				text, err := spokenText(str(obj["text"]))
				if err != nil {
					return nil, err
				}
				if b.worker == nil || !b.worker.ready.Load() {
					return nil, problem(503, "TTS unavailable")
				}
				id, err := deviceID(r, obj, false)
				if err != nil {
					return nil, err
				}
				who, _ := b.lookupDevice(id)
				who.DeviceID = id
				ctx, cancel := context.WithTimeout(ctx, ttsWait)
				defer cancel()
				pcm, err := b.worker.synthesize(ctx, b.voiceFor(who), text)
				if err != nil {
					return nil, problem(502, "TTS synthesis failed")
				}
				sendBytes(w, 200, pcmType, pcm)
				return nil, nil
			}
		}
	}
	return nil, problem(404, "Not found")
}

type otaFile struct {
	path   string
	mu     sync.Mutex
	info   os.FileInfo
	digest string
}

func (o *otaFile) sha(f *os.File, info os.FileInfo) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.info != nil && info.Size() == o.info.Size() && info.ModTime() == o.info.ModTime() && os.SameFile(info, o.info) {
		return o.digest, nil
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, info.Size()))
	if err != nil || n != info.Size() {
		return "", problem(502, "Cannot read firmware")
	}
	o.info, o.digest = info, hex.EncodeToString(h.Sum(nil))
	return o.digest, nil
}

func (o *otaFile) serve(w http.ResponseWriter, manifest bool) (any, error) {
	f, err := os.Open(o.path)
	if err != nil {
		if os.IsNotExist(err) && manifest {
			return object{"exists": false, "size": 0, "sha256": "", "mtime": 0}, nil
		}
		return nil, problem(404, "Firmware not found")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, problem(404, "Firmware not found")
	}
	digest, err := o.sha(f, info)
	if err != nil {
		return nil, err
	}
	if manifest {
		return object{"exists": true, "size": info.Size(), "sha256": digest, "mtime": info.ModTime().Unix()}, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, problem(502, "Cannot read firmware")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Firmware-Sha256", digest)
	w.WriteHeader(200)
	_, _ = io.CopyN(w, f, info.Size())
	return nil, nil
}
