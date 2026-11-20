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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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
	for _, key := range []string{"ECHOEAR_BRIDGE_TOKEN", "ECHOEAR_BRIDGE_PORT", "ECHOEAR_OPENCODE_AGENT",
		"ECHOEAR_OPENCODE_MODEL", "ECHOEAR_OPENCODE_URL", "ECHOEAR_OPENCODE_PASSWORD", "ECHOEAR_OPENCODE_DIR",
		"ECHOEAR_TTS_MODEL", "ECHOEAR_TTS_PYTHON"} {
		out[key] = env(key, out[key])
	}
	if !tokenRE.MatchString(out["ECHOEAR_BRIDGE_TOKEN"]) {
		t.Fatal("missing live bridge token")
	}
	return out
}

func liveOpenCode(t *testing.T, client *http.Client, home string, conf map[string]string) upstream {
	t.Helper()
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	api, err := openCodeUpstream(client, state, conf["ECHOEAR_OPENCODE_URL"], conf["ECHOEAR_OPENCODE_PASSWORD"])
	if err != nil {
		t.Fatal(err)
	}
	return api
}

func liveOpenCodeDir(home string, conf map[string]string) string {
	if dir := conf["ECHOEAR_OPENCODE_DIR"]; dir != "" {
		return dir
	}
	return filepath.Join(env("XDG_DATA_HOME", filepath.Join(home, ".local/share")), "echoear/opencode/workspace")
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
				if strings.HasPrefix(s.Title, "EchoEar: ") && strings.Contains(s.Title, prefix) && sessionIDRE.MatchString(s.ID) {
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
		"ECHOEAR_BRIDGE_PORT=0", "ECHOEAR_DB_PATH="+filepath.Join(state, "echoear.db"),
		"ECHOEAR_TURSO_URL=", "ECHOEAR_TURSO_AUTH_TOKEN=", "ECHOEAR_OTA_DIR="+root, "ECHOEAR_TTS_WORKER="+worker,
		"ECHOEAR_OPENCODE_AGENT="+conf["ECHOEAR_OPENCODE_AGENT"], "ECHOEAR_OPENCODE_MODEL="+conf["ECHOEAR_OPENCODE_MODEL"],
		"ECHOEAR_OPENCODE_URL="+conf["ECHOEAR_OPENCODE_URL"], "ECHOEAR_OPENCODE_PASSWORD="+conf["ECHOEAR_OPENCODE_PASSWORD"],
		"ECHOEAR_OPENCODE_DIR="+liveOpenCodeDir(home, conf), "ECHOEAR_OPENCODE_PAIR_PORT=0",
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
	conf := liveConfig(t, home)
	if conf["ECHOEAR_OPENCODE_AGENT"] == "" {
		conf["ECHOEAR_OPENCODE_AGENT"] = "echoear"
	}
	if conf["ECHOEAR_OPENCODE_MODEL"] == "" {
		conf["ECHOEAR_OPENCODE_MODEL"] = defaultOpenCodeModel
	}
	settings, err := openCodeSettings(conf["ECHOEAR_OPENCODE_AGENT"], conf["ECHOEAR_OPENCODE_MODEL"])
	if err != nil {
		t.Fatal(err)
	}
	client := pooledClient()
	client.Timeout = 115 * time.Second
	defer client.CloseIdleConnections()
	oc := liveOpenCode(t, client, home, conf)
	port := conf["ECHOEAR_BRIDGE_PORT"]
	if port == "" {
		port = "8765"
	}
	c := liveClient{client, env("ECHOEAR_TEST_URL", "http://"+net.JoinHostPort("127.0.0.1", port)), conf["ECHOEAR_BRIDGE_TOKEN"]}
	liveHealthy(t, c)
	unauthorized := c
	unauthorized.token = ""
	unauthorized.read(t, "/api/devices", nil, "", nil, 401)
	unauthorized.read(t, "/api/chat", []byte("{}"), jsonType, nil, 401)
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
				Port     int    `json:"port"`
				LastSeen int64  `json:"last_seen"`
			}
		}](t, c, "/api/devices", nil, nil, "", nil)
		devices := make(map[string]string, len(got.Devices))
		for _, p := range got.Devices {
			if p.Kind == "virtual" {
				continue
			}
			if p.DeviceID == "" || net.ParseIP(p.IP) == nil || p.Port <= 0 || p.LastSeen <= 0 || devices[p.DeviceID] != "" {
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
		for _, path := range []string{"/ota", "/ota/box3"} {
			manifest := liveJSON[struct {
				Exists bool
				Size   int64
				SHA256 string
				Mtime  int64
			}](t, c, path+"/version.json", nil, nil, "", nil)
			firmware, res := c.read(t, path+"/latest.bin", nil, "", nil, 200)
			digest := fmt.Sprintf("%x", sha256.Sum256(firmware))
			if !manifest.Exists || manifest.Mtime <= 0 || manifest.Size != int64(len(firmware)) ||
				manifest.Size != res.ContentLength || digest != manifest.SHA256 || digest != res.Header.Get("X-Firmware-Sha256") {
				t.Fatal("OTA image and manifest differ", path)
			}
			t.Logf("%s verified: %d bytes, SHA-256 %s", path, manifest.Size, digest)
		}
	})

	direct, res := c.read(t, "/api/tts", []byte(`{"text":"The Moon reflects sunlight."}`), jsonType, nil, 200)
	if res.Header.Get("Content-Type") != pcmType || !livePCMOK(direct) {
		t.Fatal("direct TTS returned invalid PCM")
	}
	t.Run("VoiceAndChat", func(t *testing.T) {
		iso := liveIsolated(t, home, data, conf, run, client)
		id := run + "-voice"
		board := testConnectWS(t, iso.base, iso.token, false)
		board.timeout = 100 * time.Second
		board.helloV2(t, id, "LiveVoice")
		viewer := testConnectWS(t, iso.base, iso.token, true)
		viewer.timeout = 100 * time.Second
		viewer.watch(t, id)
		input, _ := json.Marshal(object{"text": liveVoicePrompt})
		recording, _ := iso.read(t, "/api/tts", input, jsonType, nil, 200)
		board.json(t, object{"ev": "rec_start"})
		for offset := 0; offset < len(recording); offset += pcmPacket {
			chunk := recording[offset:min(offset+pcmPacket, len(recording))]
			board.write(t, maskedFrame(wsBinary, true, append([]byte{micPrefix}, chunk...)))
		}
		board.json(t, object{"ev": "rec_end"})
		board.until(t, hasWS("tts", `"state":"start"`))
		var spoken []byte
		end := board.until(t, func(f wsFrame) bool {
			if f.op == wsBinary && f.data[0] == pcmPrefix {
				spoken = append(spoken, f.data[1:]...)
			}
			return hasWS("tts", `"state":"end"`)(f)
		})
		if !livePCMOK(spoken) {
			t.Fatal("voice turn did not return playable PCM")
		}
		board.json(t, object{"ev": "tts_ack", "state": "done", "audio": testObject(t, end.data)["audio"]})
		board.write(t, maskedFrame(wsClose, true, wsCloseFrame(1000).data))
		board.until(t, func(f wsFrame) bool { return f.op == wsClose })
		viewer.until(t, hasWS("peers", `"ws":false`))
		entries := liveJSON[struct{ Messages []chatEntry }](t, iso, "/api/devices/"+id+"/messages", nil, nil, "", nil).Messages
		if len(entries) != 2 || entries[0].Source != "voice" || !strings.Contains(strings.ToLower(entries[0].Text), "right triangle") {
			t.Fatal("voice question and answer not in canonical history")
		}

		follow := liveJSON[struct {
			Reply     string
			SessionID string `json:"session_id"`
			AudioURL  string `json:"audio_url"`
		}](t, iso, "/api/chat", object{"device_id": id, "text": "Repeat in one sentence, without tools: Paris is the capital of France."}, nil, "", nil)
		assertLiveActor(t, oc, follow.SessionID, settings)
		if !strings.Contains(strings.ToLower(follow.Reply), "paris") || !strings.HasPrefix(follow.AudioURL, "/api/audio/") {
			t.Fatal("remembered device could not chat and play a text reply")
		}
		pcm, res := iso.read(t, follow.AudioURL, nil, "", nil, 200)
		replay, fixed := iso.read(t, follow.AudioURL, nil, "", nil, 200)
		if !livePCMOK(pcm) || res.Header.Get("Content-Type") != pcmType || !bytes.Equal(pcm, replay) || fixed.ContentLength != int64(len(pcm)) {
			t.Fatal("live chat audio was silent, corrupt, or not byte-identical on replay")
		}
		messages, err := ocData[[]struct{ Text string }](t.Context(), oc, http.MethodGet, "/api/session/"+follow.SessionID+"/message?order=asc&limit=10&type=user", nil)
		if err != nil || len(messages) != 2 || !strings.HasPrefix(messages[0].Text, "[LiveVoice ("+id+") · hardware · voice]") {
			t.Fatal("HTTP chat did not reuse the WebSocket voice session or preserve provenance")
		}
		t.Logf("validated voice capture, playback, canonical history, HTTP follow-up and %d replayed PCM bytes", len(pcm))
	})
	t.Run("DevicesAfter", func(t *testing.T) {
		if !maps.Equal(before, liveDevices(t)) {
			t.Fatal("live tests changed the deployed peer registry")
		}
		t.Logf("%d registered peer addresses unchanged", len(before))
	})
}
