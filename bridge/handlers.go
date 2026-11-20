package main

import (
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
	ota       otaFile
	box3OTA   otaFile
	ws        *wsHub
	virtual   *store[virtualClient]
	tunnels   *tunnelManager
	history   *chatHistory
	timers    *timerManager
	diag      diagnostics
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

func jsonBody(r *http.Request) (object, error) {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if kind != jsonType {
		return nil, problem(415, "Expected application/json")
	}
	lengths := r.Header.Values("Content-Length")
	if len(r.TransferEncoding) != 0 || len(lengths) > 1 {
		return nil, problem(400, "Request requires a single Content-Length")
	}
	if len(lengths) == 0 && r.ContentLength <= 0 {
		return nil, problem(411, "Content-Length required")
	}
	if len(lengths) != 0 && len(lengths[0]) > 12 {
		return nil, problem(400, "Invalid Content-Length")
	}
	if r.ContentLength > maxJSON {
		return nil, problem(413, "Request body too large")
	}
	if r.ContentLength < 0 {
		return nil, problem(400, "Invalid Content-Length")
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		var timed net.Error
		if errors.As(err, &timed) && timed.Timeout() {
			return nil, problem(408, "Request body timed out")
		}
		return nil, problem(400, "Incomplete request body")
	}
	return decodeObject(buf)
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
	return deviceSettingID(path, "voice")
}

func deviceSettingID(path, setting string) (string, bool) {
	suffix, ok := strings.CutPrefix(path, "/api/devices/")
	if !ok {
		return "", false
	}
	id, ok := strings.CutSuffix(suffix, "/"+setting)
	return id, ok && deviceIDRE.MatchString(id)
}

func (b *bridge) volumeAPI(r *http.Request, id string) (any, error) {
	args, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	auto, ok := args["auto"]
	if !ok || len(args) > 2 || len(args) == 2 && args["volume"] == nil {
		return nil, problem(400, `Expected {"auto":true} or {"auto":false,"volume":0..100}`)
	}
	if b.ws == nil {
		return nil, problem(503, "Device hub unavailable")
	}
	delete(args, "auto")
	args["auto_volume"] = auto
	return b.ws.mcpControl(r.Context(), id, args)
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
		if d := b.ws.devices[strings.ToLower(id)]; d != nil && d.drawer != "" {
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

type answer struct {
	Reply, SessionID string
	AudioURL         *string
}

func (b *bridge) attachAudio(ctx context.Context, out *answer, stream *AudioStream, start time.Time) {
	if stream != nil {
		if pcm, err := stream.chunk(ctx, 0); err == nil && len(pcm) != 0 {
			stream.traceFirst(traceFrom(ctx), start)
			if id, _, _ := b.audio.add(stream); id != "" {
				path := "/api/audio/" + id
				out.AudioURL = &path
			}
		}
	}
}

type delivery struct {
	sent, delivered, failed []string
}

func (b *bridge) relay(ctx context.Context, sender peer, others []peer, target, text string) (answer, error) {
	recipients := b.intercomTargets(others, target)
	d := b.deliver(ctx, sender, recipients, text, nil)
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
	out := answer{Reply: reply, SessionID: sid}
	start := time.Now()
	if b.worker != nil && b.worker.ready.Load() {
		if pcm, err := b.clips.get(ctx, b.worker, b.voiceFor(sender), reply); err == nil {
			s := newAudioStream()
			s.append(pcm)
			s.finish(nil)
			b.attachAudio(ctx, &out, s, start)
		}
	}
	return out, nil
}

func (b *bridge) turn(ctx context.Context, sender peer, text string) (answer, error) {
	others, err := b.peerSnapshot(sender.DeviceID)
	if err != nil {
		return answer{}, err
	}
	const source = "typed"
	if traceFrom(ctx) == nil {
		ctx = b.traceTurn(ctx, sender.DeviceID, b.catFor(sender).Name, source, time.Now())
	}
	if direct := directMessage(others, text); direct != nil {
		b.chat(sender.DeviceID, newChat("user", "You", text, source), true)
		return b.relay(ctx, sender, others, direct.target, direct.text)
	}
	options := turnOptions{source: source}
	gen, err := b.generateReply(ctx, b.root, sender, text, options)
	if err != nil {
		return answer{}, err
	}
	out := answer{Reply: gen.reply, SessionID: gen.sid}
	if gen.reply != "" {
		b.attachAudio(ctx, &out, gen.audio, gen.ttsStart)
	}
	return out, nil
}

type generatedReply struct {
	reply, sid string
	audio      *AudioStream
	ttsStart   time.Time
}

type turnOptions struct {
	source  string
	web     bool
	publish func(*AudioStream, time.Time)
}

func turnPrompt(sender peer, web bool, source, text string, cat catState) string {
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
	if traceFrom(ctx) == nil {
		ctx = b.traceTurn(ctx, sender.DeviceID, b.catFor(sender).Name, options.source, time.Now())
	}
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
	prepare := func() (string, string) {
		cat := b.catFor(sender)
		return cat.Name, turnPrompt(sender, options.web, options.source, text, cat)
	}
	raw, sid, err := b.oc.askPrepared(ctx, sender.DeviceID, prepare, onSentence, options.source, nil, func(entry chatEntry) {
		if entry.Role == "assistant" {
			entry.Sender = b.catFor(sender).Name
		}
		b.chat(sender.DeviceID, entry, true)
	})
	if err != nil {
		if eager != nil {
			eager.discard()
		}
		return generatedReply{}, err
	}
	reply := strings.TrimSpace(fenced.ReplaceAllString(raw, ""))
	ttsStart := time.Now()
	var stream *AudioStream
	clean, speechErr := spokenText(reply)
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
			options.publish(stream, ttsStart)
		}
	} else if eager != nil {
		eager.discard()
	}
	return generatedReply{reply, sid, stream, ttsStart}, nil
}

func (b *bridge) route(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	ctx := r.Context()
	switch {
	case path == "/api/cats" || strings.HasPrefix(path, "/api/cats/") ||
		strings.HasPrefix(path, "/api/devices/") && strings.HasSuffix(path, "/cat"):
		return b.catAPI(w, r, path)
	case path == "/api/memory" || path == "/api/memory/scopes":
		return b.memoryAPI(w, r, path)
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
		case path == "/api/diagnostics":
			return b.diagnosticReport(), nil
		case path == "/api/scope":
			return object{"scoped": false, "auto_volume": true, "voices": voiceCatalog}, nil
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
		if path == "/api/opencode/compact" {
			return b.compactAPI(w, r)
		}
		if id, ok := voiceDevice(path); ok {
			return b.voiceAPI(r, id)
		}
		if id, ok := deviceSettingID(path, "volume"); ok {
			return b.volumeAPI(r, id)
		}
		if path == "/api/tts" {
			obj, err := jsonBody(r)
			if err != nil {
				return nil, err
			}
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
