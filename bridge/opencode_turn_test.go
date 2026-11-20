package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpenCode(t *testing.T, ctx context.Context, dir string, handle http.HandlerFunc) *openCode {
	t.Helper()
	api := httptest.NewServer(handle)
	t.Cleanup(api.Close)
	config := filepath.Join(dir, "oc-api.json")
	if err := writeMap(config, object{"url": api.URL, "password": "unit"}); err != nil {
		t.Fatal(err)
	}
	return newOpenCode(ctx, upstream{"OpenCode", api.Client(), &fileCache[endpoint]{path: config, read: openCodeEndpoint}},
		testDB(t, ":memory:", ""), dir, ocSettings{})
}

func expectValue[T comparable](t *testing.T, result <-chan T, want T) {
	t.Helper()
	select {
	case got := <-result:
		if got != want {
			t.Fatalf("got %v, want %v", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %v", want)
	}
}

func TestOpenCodeCancelAndReset(t *testing.T) {
	for _, action := range []string{"cancel", "reset", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			const id, sid = "echoear-session-test", "ses-test"
			interrupts := make(chan string, 4)
			release := make(chan struct{})
			b, base := wsHarness(t, func(b *bridge, dir string) {
				b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/api/session/"+sid+"/interrupt" {
						w.WriteHeader(404)
						return
					}
					interrupts <- r.URL.Path
					select {
					case <-release:
						_, _ = io.WriteString(w, `{"data":{"interrupted":false}}`)
					case <-r.Context().Done():
					}
				})
				if err := b.oc.save(id, sid); err != nil {
					t.Fatal(err)
				}
			})
			c := testConnectWS(t, base, "unit", false)
			c.hello(t, id, "Cake")
			reset := make(chan error, 1)
			switch action {
			case "cancel":
				b.ws.mu.Lock()
				b.ws.cancelTurn(b.ws.devices[id])
				b.ws.mu.Unlock()
			case "reset":
				go func() {
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/reset", strings.NewReader(`{"device_id":"`+id+`"}`))
					req.Header.Set("Content-Type", jsonType)
					_, err := b.chatAPI(req, "/api/reset", "")
					reset <- err
				}()
			case "disconnect":
				c.conn.Close()
			}
			expectValue(t, interrupts, "/api/session/"+sid+"/interrupt")
			if got, err := b.oc.get(id); err != nil || got != sid {
				t.Fatalf("stored session changed before the interrupt finished: %q, %v", got, err)
			}
			close(release)
			if action == "reset" {
				expectValue[error](t, reset, nil)
			}
			if action == "reset" {
				eventually(t, func() bool { saved, _ := b.oc.get(id); return saved == "" })
			}
		})
	}
}

func TestOpenCodeInterruptOrdersReplacementPrompt(t *testing.T) {
	const id, sid = "echoear-33d8", "ses-order"
	prompts := make(chan string, 4)
	interrupt := make(chan bool, 4)
	allowPrompt, allowStop := make(chan struct{}), make(chan struct{})
	root, stop := context.WithCancel(t.Context())
	defer stop()
	o := testOpenCode(t, root, t.TempDir(), func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session":
			raw, _ := io.ReadAll(r.Body)
			if got := testObject(t, raw)["title"]; got != "EchoEar: Cake (echoear-33d8)" {
				t.Error("unexpected character title:", got)
			}
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
		case "/api/session/" + sid:
			_, _ = fmt.Fprintf(w, `{"data":{"id":%q}}`, sid)
		case "/api/session/" + sid + "/prompt":
			raw, _ := io.ReadAll(r.Body)
			text := str(testObject(t, raw)["text"])
			prompts <- text
			if text == "First" {
				select {
				case <-allowPrompt:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
		case "/api/session/" + sid + "/interrupt":
			interrupt <- true
			select {
			case <-allowStop:
				_, _ = io.WriteString(w, `{"data":{"interrupted":true}}`)
			case <-r.Context().Done():
			}
		case "/api/experimental/session/" + sid + "/wait":
			w.WriteHeader(204)
		case "/api/session/" + sid + "/message":
			_, _ = io.WriteString(w, `{"data":[{"id":"msg2","type":"assistant","finish":"stop","content":[{"type":"text","text":"Done"}]},{"id":"msg1","type":"user"}]}`)
		default:
			w.WriteHeader(404)
		}
	})
	who := peer{DeviceID: id, Character: "Cake"}
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := o.ask(root, who, "First", nil, "text", nil, nil); first <- err }()
	expectValue(t, prompts, "First")
	o.interruptDevice(id)
	go func() { _, _, err := o.ask(root, who, "Second", nil, "text", nil, nil); second <- err }()
	select {
	case <-interrupt:
		t.Fatal("interrupt raced prompt admission")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowPrompt)
	expectValue(t, interrupt, true)
	select {
	case <-prompts:
		t.Fatal("replacement prompt outran interrupt")
	case <-time.After(20 * time.Millisecond):
	}
	close(allowStop)
	expectValue(t, prompts, "Second")
	expectValue[error](t, second, nil)
	select {
	case err := <-first:
		if err == nil {
			t.Fatal("first turn did not stop")
		}
	case <-time.After(time.Second):
		t.Fatal("first turn did not return")
	}
}

func TestOpenCodeFallbackTitlesAndChat(t *testing.T) {
	const reply = "Done.\n```echoear-message\n{\"to\":\"Mint\",\"text\":\"Do not deliver\"}\n```"
	titles, prompts := make(chan string, 4), make(chan string, 4)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/session":
				raw, _ := io.ReadAll(r.Body)
				titles <- str(testObject(t, raw)["title"])
				_, _ = io.WriteString(w, `{"data":{"id":"ses-ask"}}`)
			case "/api/session/ses-ask/prompt":
				raw, _ := io.ReadAll(r.Body)
				prompts <- str(testObject(t, raw)["text"])
				_, _ = io.WriteString(w, `{"data":{"id":"msg1"}}`)
			case "/api/experimental/session/ses-ask/wait":
				w.WriteHeader(204)
			case "/api/session/ses-ask/message":
				fmt.Fprintf(w, `{"data":[{"id":"msg2","type":"assistant","finish":"stop","content":[{"type":"text","text":%q}]},{"id":"msg1","type":"user"}]}`, reply)
			default:
				w.WriteHeader(404)
			}
		})
	})
	for _, item := range []peer{{DeviceID: "echoear-empty"}, {DeviceID: "echoear-equal", Character: "echoear-equal"}} {
		if _, err := b.oc.ensureSession(t.Context(), item.DeviceID, item.Character); err != nil {
			t.Fatal(err)
		}
		expectValue(t, titles, "EchoEar: "+item.DeviceID)
	}
	const id = "echoear-ask"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	mint := testConnectWS(t, base, "unit", false)
	mint.hello(t, "echoear-mint", "Mint")
	mirror := testConnectWS(t, base, "unit", true)
	mirror.watch(t, id)
	toolTest(t, base, "emit", object{"to": id, "chime": "tap"}, false)
	c.until(t, hasWS("chime", `"kind":"tap"`))
	select {
	case created := <-titles:
		t.Fatal("local-only tap started an AI session:", created)
	default:
	}
	testRequest(t, "POST", base+"/api/chat", "unit", object{"device_id": id, "text": "Next quiz round!"}, 200)
	expectValue(t, titles, "EchoEar: Cake (echoear-ask)")
	select {
	case text := <-prompts:
		if !strings.HasSuffix(text, "Next quiz round!") {
			t.Fatal("chat did not submit its explicit prompt")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("chat did not start a turn")
	}
	if frame := mirror.until(t, hasWS("svg", "Done.")); !hasWS("svg", "Answer")(frame) {
		t.Fatal("no-audio answer notice was missing")
	}
	if got := apiObjects(t, base+"/api/devices/echoear-mint/messages", "messages"); len(got) != 0 {
		t.Fatal("fenced prose triggered intercom")
	}
}
