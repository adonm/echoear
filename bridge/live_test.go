//go:build live

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const liveVoicePrompt = "What is a right triangle? Please explain in four complete sentences."

type liveClient struct {
	client      *http.Client
	base, token string
}

func (c liveClient) open(t *testing.T, path string, body []byte, media string, headers map[string]string) *http.Response {
	t.Helper()
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("construct request for %s: %v", path, err)
	}
	if c.token != "" {
		req.Header.Set("X-EchoEar-Token", c.token)
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return res
}

func (c liveClient) read(t *testing.T, path string, body []byte, media string, headers map[string]string, status int) ([]byte, *http.Response) {
	t.Helper()
	res := c.open(t, path, body, media, headers)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != status {
		t.Fatalf("%s: HTTP %d (want %d), read error: %v", path, res.StatusCode, status, err)
	}
	return raw, res
}

func liveJSON[T any](t *testing.T, c liveClient, path string, input any, raw []byte, media string, headers map[string]string) T {
	t.Helper()
	if input != nil {
		raw, _ = json.Marshal(input)
		media = jsonType
	}
	got, _ := c.read(t, path, raw, media, headers, 200)
	var v T
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("%s returned invalid JSON: %v", path, err)
	}
	return v
}

func liveConfig(t *testing.T, home string) map[string]string {
	t.Helper()
	path := filepath.Join(env("XDG_CONFIG_HOME", filepath.Join(home, ".config")), "echoear/bridge.env")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal("cannot read deployed bridge configuration")
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || !strings.HasPrefix(key, "ECHOEAR_") {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
		} else if strings.HasPrefix(value, `'`) {
			value = strings.Trim(value, `'`)
		}
		out[key] = env(key, value)
	}
	for _, key := range []string{"ECHOEAR_BRIDGE_TOKEN", "ECHOEAR_BRIDGE_PORT", "ECHOEAR_OPENCODE_AGENT", "ECHOEAR_OPENCODE_MODEL", "ECHOEAR_TTS_MODEL", "ECHOEAR_TTS_PYTHON"} {
		out[key] = env(key, out[key])
	}
	if !tokenRE.MatchString(out["ECHOEAR_BRIDGE_TOKEN"]) {
		t.Fatal("missing live bridge token")
	}
	return out
}

func liveHealthy(t *testing.T, c liveClient) {
	t.Helper()
	health := liveJSON[object](t, c, "/health", nil, nil, "", nil)
	for _, k := range []string{"ok", "hex", "opencode", "tts"} {
		if health[k] != true {
			t.Fatalf("unhealthy live backend %s", k)
		}
	}
	if health["tts_engine"] != "kokoro-onnx" {
		t.Fatal("TTS engine metadata mismatch")
	}
}

func livePCMOK(pcm []byte) bool {
	if len(pcm) <= 8000 || len(pcm)%2 != 0 {
		return false
	}
	var power int64
	for i := 0; i < len(pcm); i += 2 {
		v := int64(int16(le.Uint16(pcm[i:])))
		power += v * v
	}
	return power > 100*100*int64(len(pcm)/2)
}

func liveSessions(t *testing.T, oc upstream, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		type session struct{ ID, Title string }
		path := "/api/session?" + url.Values{"search": {prefix}, "limit": {"50"}}.Encode()
		owned := func() ([]string, error) {
			found, err := ocData[[]session](ctx, oc, http.MethodGet, path, nil)
			var ids []string
			for _, s := range found {
				if strings.HasPrefix(s.Title, "EchoEar: "+prefix) && sessionIDRE.MatchString(s.ID) {
					ids = append(ids, s.ID)
				}
			}
			return ids, err
		}
		ids, err := owned()
		if err != nil {
			t.Errorf("cannot locate sessions for cleanup: %v", err)
			return
		}
		for _, id := range ids {
			if err := oc.jsonCall(ctx, http.MethodDelete, "/api/session/"+id, nil, nil); err != nil {
				t.Errorf("cannot delete test session: %v", err)
			}
		}
		left, err := owned()
		if len(left) != 0 || err != nil {
			t.Errorf("session cleanup: %d left (%v)", len(left), err)
		} else {
			t.Logf("deleted %d OpenCode test session(s); 0 left", len(ids))
		}
	})
}

func assertLiveActor(t *testing.T, oc upstream, sid string, settings ocSettings) {
	t.Helper()
	if !sessionIDRE.MatchString(sid) {
		t.Fatal("missing OpenCode session ID")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := ocData[[]struct {
		Agent string
		Model ocModel
	}](ctx, oc, http.MethodGet, "/api/session/"+sid+"/message?order=desc&limit=1&type=assistant", nil)
	if err != nil || len(got) != 1 || got[0].Agent != settings.Agent || got[0].Model != settings.Model {
		t.Fatalf("wrong agent or model on final message: %v", err)
	}
}

func liveIsolated(t *testing.T, home, data string, conf map[string]string, token string, client *http.Client) liveClient {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "echoear")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"hex", "echoear/tts-models"} {
		if err := os.Symlink(filepath.Join(data, rel), filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	binary := env("ECHOEAR_TEST_BIN", filepath.Join(home, ".local/share/echoear/echoear-bridge"))
	worker, err := filepath.Abs("tts_worker.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+root, "ECHOEAR_BRIDGE_TOKEN="+token, "ECHOEAR_BRIDGE_HOST=127.0.0.1",
		"ECHOEAR_BRIDGE_PORT=0", "ECHOEAR_OTA_DIR="+root, "ECHOEAR_TTS_WORKER="+worker,
		"ECHOEAR_OPENCODE_AGENT="+conf["ECHOEAR_OPENCODE_AGENT"], "ECHOEAR_OPENCODE_MODEL="+conf["ECHOEAR_OPENCODE_MODEL"],
		"ECHOEAR_TTS_MODEL="+conf["ECHOEAR_TTS_MODEL"],
		"ECHOEAR_TTS_PYTHON="+env("ECHOEAR_TTS_PYTHON", filepath.Join(home, ".local/share/echoear/tts-venv/bin/python")))
	if conf["ECHOEAR_TTS_PYTHON"] != "" {
		cmd.Env = append(cmd.Env, "ECHOEAR_TTS_PYTHON="+conf["ECHOEAR_TTS_PYTHON"])
	}
	logs, err := cmd.StderrPipe()
	if err != nil || cmd.Start() != nil {
		t.Fatal("cannot start isolated Go bridge")
	}
	addr, done := make(chan string, 1), make(chan error, 1)
	go func() {
		s := bufio.NewScanner(logs)
		for s.Scan() {
			if _, a, ok := strings.Cut(s.Text(), "EchoEar bridge listening on "); ok {
				addr <- a
			}
		}
		done <- cmd.Wait()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("isolated bridge exited: %v", err)
			}
		case <-time.After(13 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("isolated bridge failed graceful shutdown")
		}
	})
	var c liveClient
	select {
	case a := <-addr:
		c = liveClient{client, "http://" + a, token}
	case <-time.After(5 * time.Second):
		t.Fatal("isolated bridge did not bind")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h := liveJSON[object](t, c, "/health", nil, nil, "", nil); h["tts"] == true {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("isolated bridge TTS did not warm")
	return liveClient{}
}

func TestLive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	oc := upstream{"OpenCode", pooledClient(),
		&fileCache[endpoint]{path: filepath.Join(state, "opencode/service.json"), read: openCodeEndpoint}}
	conf := liveConfig(t, home)
	if conf["ECHOEAR_OPENCODE_AGENT"] == "" {
		conf["ECHOEAR_OPENCODE_AGENT"] = "echoear"
	}
	if conf["ECHOEAR_OPENCODE_MODEL"] == "" {
		conf["ECHOEAR_OPENCODE_MODEL"] = "opencode/gemini-4-neon#high"
	}
	settings, err := openCodeSettings(conf["ECHOEAR_OPENCODE_AGENT"], conf["ECHOEAR_OPENCODE_MODEL"])
	if err != nil {
		t.Fatal(err)
	}
	client := pooledClient()
	client.Timeout = 115 * time.Second
	defer client.CloseIdleConnections()
	port := conf["ECHOEAR_BRIDGE_PORT"]
	if port == "" {
		port = "8765"
	}
	c := liveClient{client, env("ECHOEAR_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port)), conf["ECHOEAR_BRIDGE_TOKEN"]}
	liveHealthy(t, c)
	unauthorized := c
	unauthorized.token = ""
	unauthorized.read(t, "/api/devices", nil, "", nil, 401)
	unauthorized.read(t, "/api/intercom", []byte("{}"), jsonType, nil, 401)
	unauthorized.read(t, "/ota/latest.bin", nil, "", nil, 401)
	run := "echoear-live-" + hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 36)))
	liveSessions(t, oc, run)

	liveDevices := func(t *testing.T) map[string]string {
		t.Helper()
		got := liveJSON[struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
				IP       string `json:"ip"`
				Kind     string `json:"kind"`
			}
		}](t, c, "/api/devices", nil, nil, "", nil)
		devices := make(map[string]string, len(got.Devices))
		for _, p := range got.Devices {
			if p.Kind == "virtual" {
				continue
			}
			if p.DeviceID == "" || net.ParseIP(p.IP) == nil || devices[p.DeviceID] != "" {
				t.Fatal("invalid or duplicate peer in live registry")
			}
			devices[p.DeviceID] = p.IP
		}
		return devices
	}
	var before map[string]string
	t.Run("Devices", func(t *testing.T) {
		before = liveDevices(t)
		t.Logf("%d registered peer addresses", len(before))
	})
	t.Run("OTA", func(t *testing.T) {
		manifest := liveJSON[struct {
			Exists bool
			Size   int64
			SHA256 string
			Mtime  int64
		}](t, c, "/ota/version.json", nil, nil, "", nil)
		firmware, res := c.read(t, "/ota/latest.bin", nil, "", nil, 200)
		digest := fmt.Sprintf("%x", sha256.Sum256(firmware))
		if !manifest.Exists || manifest.Mtime <= 0 || manifest.Size != int64(len(firmware)) ||
			manifest.Size != res.ContentLength || digest != manifest.SHA256 || digest != res.Header.Get("X-Firmware-Sha256") {
			t.Fatal("OTA image and manifest differ")
		}
		t.Logf("OTA verified: %d bytes, SHA-256 %s", manifest.Size, digest)
	})

	direct, res := c.read(t, "/api/tts", []byte(`{"text":"The Moon reflects sunlight."}`), jsonType, nil, 200)
	if res.Header.Get("Content-Type") != pcmType || !livePCMOK(direct) {
		t.Fatal("direct TTS returned invalid PCM")
	}
	var recording []byte
	if path := os.Getenv("ECHOEAR_TEST_WAV"); path != "" {
		wav, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read test WAV", err)
		}
		recording, err = wavPCM(wav)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		in, _ := json.Marshal(object{"text": liveVoicePrompt})
		recording, _ = c.read(t, "/api/tts", in, jsonType, nil, 200)
	}
	t.Run("Voice", func(t *testing.T) {
		device := run + "-voice"
		t.Cleanup(func() {
			liveJSON[object](t, c, "/api/reset", object{"device_id": device}, nil, "", nil)
		})
		start := time.Now()
		got := liveJSON[voiceAnswer](t, c, "/api/voice", nil, recording, pcmType, map[string]string{"X-Device-Id": device})
		jsonMS := time.Since(start).Milliseconds()
		if !strings.Contains(strings.ToLower(got.Transcript), "right triangle") ||
			got.AudioURL == nil || got.SampleRate != sampleRate || got.TTSFirstMS != got.TTSMS {
			t.Fatalf("voice response did not contain transcript and audio (speech=%t audio=%t)",
				strings.Contains(strings.ToLower(got.Transcript), "right triangle"), got.AudioURL != nil)
		}
		pcm, audio := c.read(t, *got.AudioURL, nil, "", nil, 200)
		total := time.Since(start).Milliseconds()
		replay, fixed := c.read(t, *got.AudioURL, nil, "", nil, 200)
		if !livePCMOK(pcm) || !bytes.Equal(pcm, replay) || fixed.ContentLength != int64(len(pcm)) {
			t.Fatal("live audio was silent, corrupt, or not byte-identical on replay")
		}
		if got.AudioStreaming && got.AudioBytes != 0 || !got.AudioStreaming && got.AudioBytes != len(pcm) ||
			slices.Contains(audio.TransferEncoding, "chunked") && !got.AudioStreaming {
			t.Fatal("inconsistent stream metadata")
		}
		t.Logf("hex_ms=%d opencode_ms=%d tts_first_ms=%d json_ms=%d total_ms=%d chunked=%t pcm_bytes=%d",
			got.HexMS, got.OpenCodeMS, got.TTSFirstMS, jsonMS, total, slices.Contains(audio.TransferEncoding, "chunked"), len(pcm))
		assertLiveActor(t, oc, got.SessionID, settings)
		// An unpunctuated tail cannot be queued until the final message is verified.
		follow := liveJSON[answer](t, c, "/api/text", object{"device_id": device, "text": "Please repeat the following text verbatim, with no diagram and no punctuation after the last word:\nParis is the capital of France. The city has a famous tower. People can walk along the river and see bridges reaching across the water"}, nil, "", nil)
		if follow.SessionID != got.SessionID || !strings.Contains(strings.ToLower(follow.Reply), "paris") || follow.AudioURL == nil {
			t.Fatal("follow-up did not reuse the session")
		}
		pcm, audio = c.read(t, *follow.AudioURL, nil, "", nil, 200)
		replay, fixed = c.read(t, *follow.AudioURL, nil, "", nil, 200)
		if !livePCMOK(pcm) || !bytes.Equal(pcm, replay) || fixed.ContentLength != int64(len(pcm)) {
			t.Fatal("live follow-up audio did not replay intact")
		}
		chunked := slices.Contains(audio.TransferEncoding, "chunked")
		t.Logf("follow_up opencode_ms=%d tts_first_ms=%d chunked=%t replay_bytes=%d", follow.OpenCodeMS, follow.TTSFirstMS, chunked, len(replay))
		liveJSON[object](t, c, "/api/reset", object{"device_id": device}, nil, "", nil)
		silence := liveJSON[voiceAnswer](t, c, "/api/voice", nil, make([]byte, 64000), pcmType, map[string]string{"X-Device-Id": device})
		if silence.Transcript != "" || silence.SessionID != "" || silence.AudioURL != nil {
			t.Fatal("silence opened an OpenCode session")
		}
	})
	t.Run("Intercom", func(t *testing.T) {
		iso := liveIsolated(t, home, data, conf, run, client)
		type deliveryPart struct {
			path string
			raw  []byte
		}
		received := make(chan deliveryPart, 8)
		peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-EchoEar-Token") != run {
				w.WriteHeader(401)
				return
			}
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxPeerPCM+1))
			if err != nil || len(raw) > maxPeerPCM {
				w.WriteHeader(400)
				return
			}
			received <- deliveryPart{r.URL.Path, raw}
			w.WriteHeader(204)
		}))
		defer peerServer.Close()
		_, port, _ := net.SplitHostPort(peerServer.Listener.Addr().String())
		p, _ := strconv.Atoi(port)
		sender, target := run+"-cake", run+"-mock"
		liveJSON[object](t, iso, "/api/register", object{"device_id": target, "character": "MockPeer", "port": p}, nil, "", nil)
		liveJSON[object](t, iso, "/api/register", object{"device_id": sender, "character": "Cake", "ip": "127.0.0.2"}, nil, "", nil)
		checkDelivery := func() []byte {
			t.Helper()
			parts := make(map[string][]byte)
			for range 2 {
				select {
				case d := <-received:
					parts[d.path] = d.raw
				case <-time.After(time.Second):
					t.Fatal("timed out waiting for peer delivery")
				}
			}
			var card struct {
				Title  string
				Shapes []map[string]any
			}
			if err := json.Unmarshal(parts["/display"], &card); err != nil || card.Title != "From Cake!" || len(card.Shapes) == 0 {
				t.Fatal("missing or invalid peer display")
			}
			btn := card.Shapes[len(card.Shapes)-1]
			audio := parts["/speak"]
			if btn["s"] != "Reply to Cake" || btn["reply_to"] != sender || !bytes.HasPrefix(audio, MakeIntercomChimePCM()) || !livePCMOK(audio) {
				t.Fatal("invalid reply button or intercom audio")
			}
			return audio
		}
		voice := liveJSON[voiceAnswer](t, iso, "/api/intercom", nil, recording, pcmType, map[string]string{"X-Device-Id": sender, "X-Target": target})
		if voice.OK == nil || !*voice.OK || voice.OpenCodeMS != 0 || !bytes.HasSuffix(checkDelivery(), NormalizeVoicePCM(recording)) {
			t.Fatal("raw voice intercom regression")
		}
		start := time.Now()
		fast := liveJSON[answer](t, iso, "/api/text", object{"device_id": sender, "text": "Tell MockPeer dinner is ready!"}, nil, "", nil)
		elapsed := time.Since(start).Milliseconds()
		if fast.SessionID != "" || fast.OpenCodeMS != 0 || fast.SentTo == nil || !slices.Equal(*fast.SentTo, []string{target}) {
			t.Fatal("fast path did not deliver or called OpenCode")
		}
		checkDelivery()
		t.Logf("fast_path_intercom_ms=%d tts_first_ms=%d", elapsed, fast.TTSFirstMS)
		start = time.Now()
		ai := liveJSON[answer](t, iso, "/api/text", object{"device_id": sender,
			"text": "Send MockPeer a short dinosaur joke! This isolated legacy client has no tool access to MockPeer. Use the legacy fenced echoear-message JSON with to and text, not tools."},
			nil, "", nil)
		if ai.SentTo == nil || !slices.Equal(*ai.SentTo, []string{target}) || strings.Contains(ai.Reply, "```") {
			t.Fatal("AI echoear-message was not delivered or leaked into reply")
		}
		checkDelivery()
		t.Logf("ai_intercom_ms=%d opencode_ms=%d tts_first_ms=%d", time.Since(start).Milliseconds(), ai.OpenCodeMS, ai.TTSFirstMS)
		assertLiveActor(t, oc, ai.SessionID, settings)
		prompt, err := ocData[[]struct{ Text string }](t.Context(), oc, http.MethodGet, "/api/session/"+ai.SessionID+"/message?order=desc&limit=1&type=user", nil)
		if err != nil || len(prompt) != 1 || !strings.HasPrefix(prompt[0].Text,
			"[EchoEar: you are Cake (device_id="+sender+"); other EchoEars: MockPeer ("+target+")]\n") {
			t.Fatal("OpenCode peer prompt context lost")
		}
		reg := liveJSON[struct {
			Devices []struct {
				DeviceID string `json:"device_id"`
				IP       string `json:"ip"`
			}
		}](t, iso, "/api/devices", nil, nil, "", nil)
		for _, who := range reg.Devices {
			if who.DeviceID == sender && who.IP == "127.0.0.2" {
				return
			}
		}
		t.Fatal("traffic overwrote the sender's registered IP")
	})
	t.Run("DevicesAfter", func(t *testing.T) {
		if !maps.Equal(before, liveDevices(t)) {
			t.Fatal("live tests changed the deployed peer registry")
		}
		t.Logf("%d registered peer addresses unchanged", len(before))
	})
}
