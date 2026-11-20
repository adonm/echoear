package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

var modelRE = regexp.MustCompile(`^([^/]+)/([^#]+)(?:#(.+))?$`)
var tokenRE = regexp.MustCompile(`^[!-~]+$`)

func openCodeSettings(agent, spec string) (ocSettings, error) {
	m := modelRE.FindStringSubmatch(spec)
	if m == nil {
		return ocSettings{}, errors.New("ECHOEAR_OPENCODE_MODEL must be provider/model or provider/model#variant")
	}
	s := ocSettings{agent, ocModel{m[1], m[2], m[3]}, []ocPermission{{"*", "*", "deny"}}}
	if agent == "echoear" {
		s.Permissions = append(s.Permissions, ocPermission{"webfetch", "*", "allow"})
	}
	return s, nil
}

func run() error {
	token := os.Getenv("ECHOEAR_BRIDGE_TOKEN")
	if !tokenRE.MatchString(token) {
		return errors.New("set ECHOEAR_BRIDGE_TOKEN to a nonempty ASCII token without whitespace")
	}
	settings, err := openCodeSettings(env("ECHOEAR_OPENCODE_AGENT", "echoear"), env("ECHOEAR_OPENCODE_MODEL", "opencode/gemini-4-neon#high"))
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	workerPath, err := filepath.Abs(env("ECHOEAR_TTS_WORKER", "tts_worker.py"))
	if err != nil {
		return err
	}
	project := filepath.Dir(workerPath)
	local := filepath.Join(data, "echoear")
	ports := env("ECHOEAR_BRIDGE_PORT", "8765")
	if port, err := strconv.Atoi(ports); err != nil || port < 0 || port > 65535 {
		return errors.New("invalid ECHOEAR_BRIDGE_PORT")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(env("ECHOEAR_BRIDGE_HOST", "0.0.0.0"), ports))
	if err != nil {
		return err
	}
	root, finish := context.WithCancel(context.Background())
	defer finish()
	oc := newOpenCode(root, upstream{"OpenCode", pooledClient(),
		&fileCache[endpoint]{path: filepath.Join(state, "opencode/service.json"), read: openCodeEndpoint}},
		filepath.Join(local, "sessions.json"), project, settings)
	hex := hexClient{upstream{"Hex", pooledClient(), &fileCache[endpoint]{path: filepath.Join(data, "hex/local-api.json"), read: hexEndpoint}},
		&fileCache[string]{path: filepath.Join(data, "hex/settings.json"), read: hexQuery}}
	models := filepath.Join(local, "tts-models")
	model := env("ECHOEAR_TTS_MODEL", "kokoro-v1.0.onnx")
	if filepath.Base(model) == model {
		model = filepath.Join(models, model)
	}
	python := env("ECHOEAR_TTS_PYTHON", filepath.Join(home, ".local/share/echoear/tts-venv/bin/python"))
	worker := newSynthWorker(root, python, workerPath, model, filepath.Join(models, "voices-v1.0.bin"))
	b := &bridge{root: root, token: token, peers: loadPeers(filepath.Join(local, "devices.json")),
		hex: hex, oc: oc, worker: worker, chime: MakeIntercomChimePCM(), peerHTTP: pooledClient(),
		ota: otaFile{path: filepath.Join(env("ECHOEAR_OTA_DIR", filepath.Join(project, "../firmware/build")), "echoear.bin")}}
	b.ws = newWSHub(b)
	go func() {
		ctx, cancel := context.WithTimeout(root, ttsWait)
		defer cancel()
		_, _ = b.clips.get(ctx, worker, "Sent to everyone!")
		others, _ := b.peers.snapshot("")
		names := map[string]bool{}
		for _, p := range others {
			if !names[p.Character] {
				names[p.Character] = true
				b.warmCharacter(ctx, p.Character)
			}
		}
	}()
	server := &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 105 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("EchoEar bridge listening on %s", listener.Addr())
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case <-signals.Done():
		b.ws.shutdown()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
		}
		err = <-served
	case err = <-served:
	}
	finish()
	b.ws.shutdown()
	<-worker.done
	oc.client.CloseIdleConnections()
	hex.client.CloseIdleConnections()
	b.peerHTTP.CloseIdleConnections()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
