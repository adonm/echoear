package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTurnPromptIdentifiesDeviceAndInput(t *testing.T) {
	for _, tc := range []struct {
		id, source, kind string
		web              bool
	}{
		{"echoear-33d8", "voice", "hardware", false},
		{"virt-1234abcd", "typed", "web", false},
		{"phone-1234", "tap", "web", false},
		{"other-id", "tap", "web", true},
	} {
		if got := turnPrompt(peer{DeviceID: tc.id, Character: "Cake"}, tc.web, tc.source, "Hi"); got !=
			"[EchoEar: Cake ("+tc.id+"), "+tc.kind+", "+tc.source+"]\nHi" {
			t.Fatal(got)
		}
	}
}

func TestPayloads(t *testing.T) {
	button := object{"t": "btn", "x": -1, "y": 360, "w": 180, "h": 40, "c": "#abcdef", "s": "Explore",
		"tap": "**Hi**", "ask": strings.Repeat("x", 130), "reply_to": "**echoear-ABCD**"}
	spec := []object{button, {"t": "circle", "x": 0, "y": 1, "r": 2, "c": "bad"},
		{"t": "arc", "x": 0, "y": 0, "r": 10, "a1": -3, "a2": 361, "c": "#ffffff"}}
	for range 30 {
		spec = append(spec, object{"t": "text", "x": 12.6, "y": 200, "c": "#010203", "s": "Large", "ask": strings.Repeat("🔥", 120)})
	}
	raw, _ := json.Marshal(object{"shapes": spec})
	reply, screen, message := extractPayloads("Hi!\n```echoear-message\n{\"to\":\"Mint\",\"text\":\"**Hi**\"}\n```\n```echoear-screen\n" + string(raw) + "\n```")
	if reply != "Hi!" || message == nil || *message != (intercomMessage{"Mint", "Hi"}) || screen == nil {
		t.Fatalf("bad payload extraction: %q %v %v", reply, screen != nil, message)
	}
	want := object{"t": "btn", "x": 0, "y": 359, "w": 180, "h": 40, "c": "#ABCDEF", "s": "Explore",
		"tap": "Hi", "ask": strings.Repeat("x", 120), "reply_to": "echoear-ABCD"}
	shapes := screen["shapes"].([]object)
	encoded, _ := json.Marshal(screen)
	if len(shapes) < 3 || len(shapes) > 24 || len(encoded) >= maxScreen || !reflect.DeepEqual(shapes[0], want) {
		t.Fatalf("bad sanitizer: %d shapes %d bytes, button %v", len(shapes), len(encoded), shapes[0])
	}
	if shapes[1]["a1"] != 0 || shapes[1]["a2"] != 360 || shapes[2]["x"] != 12 {
		t.Fatalf("bad numeric coordinates: %v %v", shapes[1], shapes[2]["x"])
	}
	tooMany := slices.Repeat([]object{button}, 40)
	for i := range tooMany {
		tooMany[i] = object{"t": "line", "x1": 0, "y1": 1, "x2": 3, "y2": 4, "c": "#123456", "reply_to": "invalid"}
	}
	raw, _ = json.Marshal(object{"shapes": tooMany})
	limited := sanitizeScreen(string(raw))["shapes"].([]object)
	if len(limited) != 24 || limited[0]["reply_to"] != nil {
		t.Fatal("shape limit or non-button action")
	}
	for _, bad := range []string{"```echoear-screen\n{\"title\":\"unclosed\"}", "```echoear-message\n{bad}\n```"} {
		if r, s, m := extractPayloads("Hi!\n" + bad); r != "Hi!" || s != nil || m != nil {
			t.Fatalf("accepted bad fence: %q", bad)
		}
	}
	card := messageCard(peer{DeviceID: "echoear-ABCD", Character: "Cake"}, "Hi")
	if parts := card["shapes"].([]object); parts[len(parts)-1]["reply_to"] != "echoear-ABCD" {
		t.Fatal("reply not routed to sender ID")
	}
}

func TestSentencesAndFence(t *testing.T) {
	want := []string{"Whoa! Dr. Lee found a 3.2-inch shell.", "Ooh, awesome question! What made it spiral?"}
	if got := SplitSentences("**Whoa!** Dr. Lee found a 3.2-inch shell. Ooh, awesome question! What made it spiral?"); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	var s sentenceExtractor
	var streamed []string
	for _, delta := range []string{"**Whoa!** Dr. Lee found a 3.", "2-inch shell. ", "Ooh, awesome question!", " What made it spiral in circles?\n", "``", "`echoear-screen", "\nNever say this. \n```", "Not eager even after fence. "} {
		streamed = append(streamed, s.add(delta)...)
	}
	want[1] = "Ooh, awesome question! What made it spiral in circles?"
	if !s.halted || !slices.Equal(streamed, want) {
		t.Fatal(streamed)
	}
}

func sineFloats(n, hz int, gain float64) []byte {
	raw := make([]byte, n*4)
	for i := range n {
		le.PutUint32(raw[4*i:], math.Float32bits(float32(gain*math.Sin(2*math.Pi*float64(hz*i)/24000))))
	}
	return raw
}

func TestAudioDSP(t *testing.T) {
	rms := func(pcm []byte) float64 {
		var sum float64
		for i := 0; i < len(pcm); i += 2 {
			v := float64(int16(le.Uint16(pcm[i:])))
			sum += v * v
		}
		return math.Sqrt(sum / float64(len(pcm)/2))
	}
	pcm, err := synthPCM(sineFloats(24000, 1000, .25), 24000)
	if err != nil || len(pcm) != 32000 || math.Abs(rms(pcm)-10426) > 100 {
		t.Fatalf("resampler passband: %d bytes, error %v", len(pcm), err)
	}
	alias, err := synthPCM(sineFloats(24000, 10000, .25), 24000)
	if err != nil || rms(alias[64:len(alias)-64]) > 110 {
		t.Fatalf("resampler aliasing: %v", err)
	}
	loud := bytes.Repeat([]byte{0x80, 0x3e}, 60)
	quiet := bytes.Repeat([]byte{64, 0}, 120)
	normalized := int16(le.Uint16(NormalizeVoicePCM(loud)))
	if normalized < 25000 || normalized > 26100 || !bytes.Equal(NormalizeVoicePCM(quiet), quiet) {
		t.Fatal("bad normalization or noise-floor guard")
	}
	wav := WrapWAV(pcm)
	observed, err := wavPCM(wav)
	if err != nil || !bytes.Equal(pcm, observed) || len(wav) != 44+len(pcm) {
		t.Fatal("WAV round trip", err)
	}
	if a := MakeIntercomChimePCM(); len(a) != 7040 || rms(a) < 1000 {
		t.Fatal("bad intercom chime")
	}
}

func BenchmarkSincOneSecond(b *testing.B) {
	raw := sineFloats(24000, 1000, .25)
	for b.Loop() {
		_, _ = synthPCM(raw, 24000)
	}
}

func TestAudioStream(t *testing.T) {
	b := &bridge{token: "unit"}
	srv := httptest.NewServer(b)
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 2 * time.Second
	first, last := bytes.Repeat([]byte{1, 0}, 32), bytes.Repeat([]byte{2, 0}, 32)
	open := func(id string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/audio/"+id, nil)
		req.Header.Set("X-EchoEar-Token", "unit")
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	for _, fail := range []bool{false, true} {
		s := newAudioStream()
		woke := make(chan bool, 1)
		go func() { chunk, err := s.chunk(t.Context(), 0); woke <- err == nil && bytes.Equal(chunk, first) }()
		if !s.append(first) || !<-woke {
			t.Fatal("first chunk did not wake reader")
		}
		id, size, streaming := b.audio.add(s)
		if id == "" || size != 0 || !streaming {
			t.Fatal("not streaming")
		}
		res := open(id)
		if res.StatusCode != 200 || !slices.Contains(res.TransferEncoding, "chunked") {
			t.Fatal("missing chunked framing")
		}
		got := make([]byte, len(first))
		if _, err := io.ReadFull(res.Body, got); err != nil || !bytes.Equal(got, first) {
			t.Fatalf("first chunk: %v", err)
		}
		if fail {
			s.finish(errTTS)
		} else {
			s.append(last)
			s.finish(nil)
		}
		tail, err := io.ReadAll(res.Body)
		res.Body.Close()
		replay := open(id)
		pcm, replayErr := io.ReadAll(replay.Body)
		replay.Body.Close()
		if fail && (err != io.ErrUnexpectedEOF || replay.StatusCode != 404) {
			t.Fatalf("failed stream persisted or looked complete: %v HTTP %d", err, replay.StatusCode)
		}
		if !fail && (err != nil || replayErr != nil || !bytes.Equal(tail, last) || replay.ContentLength != int64(len(first)+len(last)) ||
			!bytes.Equal(pcm, append(slices.Clone(first), last...))) {
			t.Fatalf("bad cached replay (%v / %v)", err, replayErr)
		}
	}
	stall := newAudioStream()
	if _, err := stall.waitChunk(t.Context(), 0, 5*time.Millisecond); err == nil {
		t.Fatal("stalled stream did not time out")
	}
	if id, _, _ := b.audio.add(stall); id != "" {
		t.Fatal("cached timed-out stream")
	}
	ids := make([]string, 17)
	for i := range ids {
		s := newAudioStream()
		s.append(first)
		s.finish(nil)
		ids[i], _, _ = b.audio.add(s)
		if i == 15 {
			res := open(ids[0])
			res.Body.Close()
		}
	}
	for i, want := range []int{200, 404} {
		res := open(ids[i])
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("LRU item %d: %d != %d", i, res.StatusCode, want)
		}
	}
}

func TestStateAndDirectMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "echoear.db")
	db := testDB(t, path, "")
	p := peers{deviceStore(db)}
	mint := peer{"echoear-mint", "Mint", "127.0.0.2", 8080, time.Now().Unix()}
	if err := p.register(mint); err != nil {
		t.Fatal(err)
	}
	got, err := p.touch(mint.DeviceID, "127.0.0.3", "")
	if err != nil || got.IP != mint.IP || got.Port != mint.Port {
		t.Fatalf("traffic overwrote address: %+v %v", got, err)
	}
	mint = got
	if _, err := p.touch("local", "127.0.0.1", ""); err != nil {
		t.Fatal(err)
	}
	others, err := (peers{deviceStore(db)}).snapshot("cake")
	if err != nil || !slices.Equal(targets(others, "everyone"), []peer{mint}) {
		t.Fatalf("bad peer snapshot %+v %v", others, err)
	}
	for _, cmd := range []string{"Hey EchoEar, tell MINT: dinner is ready!", "Tell Mint — dinner is ready!", "Send echoear-mint, dinner is ready!"} {
		m := directMessage(others, cmd)
		if m == nil || m.text != "dinner is ready!" || len(targets(others, m.target)) != 1 {
			t.Fatalf("didn't match direct relay: %s", cmd)
		}
	}
	for _, phrase := range []string{"a joke", "a short dinosaur story", "a space fact", "about the Moon", "a riddle", "a quiz"} {
		if directMessage(others, "Send Mint "+phrase) != nil {
			t.Fatalf("bypassed composition: %s", phrase)
		}
	}
	mint.IP = "127.0.0.4"
	if err := p.register(mint); err != nil {
		t.Fatal(err)
	}
	next, err := (peers{deviceStore(db)}).snapshot("")
	if err != nil || !slices.Equal(next, []peer{mint}) || others[0].IP == mint.IP {
		t.Fatal("register didn't overwrite or modified prior snapshot")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("state mode", err)
	}
}

func TestBridgeAuthAndBody(t *testing.T) {
	b := &bridge{token: "correct", peers: peers{deviceStore(testDB(t, ":memory:", ""))}}
	for _, path := range []string{"/api/devices", "/api/audio/unknown", "/api/intercom", "/ota/version.json"} {
		for _, tokens := range [][]string{nil, {"wrong"}, {"correct", "correct"}} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			for _, token := range tokens {
				req.Header.Add("X-EchoEar-Token", token)
			}
			res := httptest.NewRecorder()
			b.ServeHTTP(res, req)
			if res.Code != 401 || res.Body.String() != `{"error":"Unauthorized"}` {
				t.Fatalf("%s: HTTP %d: %s", path, res.Code, res.Body)
			}
		}
	}
	for _, c := range []struct {
		body, kind string
		code       int
	}{
		{`{"device_id":"mint","character":"Mint","ip":"127.0.0.2"}`, jsonType, 200},
		{`{"device_id":"mint","character":"Mint","port":80.0}`, jsonType, 400},
		{`{"device_id":"mint","character":"Mint","ip":"http://127.0.0.1"}`, jsonType, 400},
		{`[]`, jsonType, 400},
		{`{} trailing`, jsonType, 400},
		{`{}`, pcmType, 415},
		{strings.Repeat(" ", maxJSON+1), jsonType, 413},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/register", strings.NewReader(c.body))
		req.RemoteAddr = "127.0.0.1:10000"
		req.Header.Set("Content-Type", c.kind)
		req.Header.Set("X-EchoEar-Token", "correct")
		res := httptest.NewRecorder()
		b.ServeHTTP(res, req)
		if res.Code != c.code {
			t.Fatalf("%s: HTTP %d instead of %d: %s", truncate(c.body, 64), res.Code, c.code, res.Body)
		}
	}
}

func TestOpenCodeStreamingAndFallback(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(strconv.FormatBool(disconnect), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			kick, early := make(chan struct{}), make(chan struct{})
			const first = "Some ocean animals make their own light."
			const reply = first + " They use it to find food.\n```echoear-screen\n{\"title\":\"Ocean\"}\n```"
			var waits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/event":
					w.Header().Set("Content-Type", "text/event-stream")
					f := w.(http.Flusher)
					_, _ = io.WriteString(w, "data: {\"type\":\"server.connected\"}\n\n")
					f.Flush()
					select {
					case <-kick:
					case <-r.Context().Done():
						return
					}
					for _, delta := range []string{"Some ocean animals ", "make their own light. "} {
						fmt.Fprintf(w, "event: message\ndata: {\"type\":\"session.text.delta\",\"data\":{\"sessionID\":\"ses1\",\"assistantMessageID\":\"msg2\",\"ordinal\":1,\"delta\":%q}}\n\n", delta)
						f.Flush()
					}
					select {
					case <-early:
					case <-r.Context().Done():
						return
					}
					if disconnect {
						return
					}
					_, _ = io.WriteString(w, "data: {\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses1\"}}\n\n")
					f.Flush()
					<-r.Context().Done()
				case "/api/session/ses1/prompt":
					close(kick)
					_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
				case "/api/session/ses1/message":
					_ = json.NewEncoder(w).Encode(object{"data": []object{
						{"id": "idle", "type": "idle", "outcome": "succeeded"},
						{"id": "msg2", "type": "assistant", "finish": "stop", "content": []object{{"type": "text", "text": reply}}},
						{"id": "msg1", "type": "user"},
					}})
				case "/api/experimental/session/ses1/wait":
					waits.Add(1)
					w.WriteHeader(204)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			ctx, cancelFeed := context.WithCancel(ctx)
			defer cancelFeed()
			path := filepath.Join(t.TempDir(), "service.json")
			if err := writeMap(path, object{"url": srv.URL, "password": "unit"}); err != nil {
				t.Fatal(err)
			}
			o := newOpenCode(ctx, upstream{"OpenCode", pooledClient(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}},
				testDB(t, ":memory:", ""), t.TempDir(), ocSettings{})
			var heard []string
			got, err := o.RunTurnStreaming(ctx, "ses1", "test", func(s string) {
				heard = append(heard, s)
				if len(heard) == 1 {
					close(early)
				}
			})
			if got != reply || err != nil || !slices.Equal(heard, []string{first}) {
				t.Fatalf("didn't overlap or lost final: %q %v %v", got, heard, err)
			}
			if (waits.Load() == 1) != disconnect {
				t.Fatalf("unexpected /wait calls: %d", waits.Load())
			}
		})
	}
}

func TestCredentialReloadOnUnauthorized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-api.json")
	var replacement []byte
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Header.Get("Authorization") == "Bearer new" {
			_, _ = io.WriteString(w, "{}")
			return
		}
		info, _ := os.Stat(path)
		if err := os.WriteFile(path, replacement, 0o600); err == nil {
			_ = os.Chtimes(path, info.ModTime(), info.ModTime())
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(strings.TrimPrefix(srv.URL, "http://127.0.0.1:"))
	replacement, _ = json.Marshal(object{"port": port, "token": "new"})
	if err := writeMap(path, object{"port": port, "token": "old"}); err != nil {
		t.Fatal(err)
	}
	u := upstream{"Hex", pooledClient(), &fileCache[endpoint]{path: path, read: hexEndpoint}}
	if !u.reachable(t.Context(), "/health") || attempts.Load() != 2 {
		t.Fatalf("unauthorized did not reload credentials: %d requests", attempts.Load())
	}
}
