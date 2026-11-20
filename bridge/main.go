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

const defaultOpenCodeModel = "opencode/gemini-4-hydrogen#low"

var modelRE = regexp.MustCompile(`^([^/]+)/([^#]+)(?:#(.+))?$`)
var tokenRE = regexp.MustCompile(`^[!-~]+$`)

func openCodeSettings(agent, spec string) (ocSettings, error) {
	m := modelRE.FindStringSubmatch(spec)
	if m == nil {
		return ocSettings{}, errors.New("ECHOEAR_OPENCODE_MODEL must be provider/model or provider/model#variant")
	}
	s := ocSettings{agent, ocModel{m[1], m[2], m[3]}, []ocPermission{{"*", "*", "deny"}}}
	if agent == "echoear" {
		s.Permissions = append(s.Permissions, ocPermission{"websearch", "*", "allow"},
			ocPermission{"echoear_*", "*", "allow"}, ocPermission{"execute", "*", "allow"})
	}
	return s, nil
}

func run() error {
	token := os.Getenv("ECHOEAR_BRIDGE_TOKEN")
	if !tokenRE.MatchString(token) {
		return errors.New("set ECHOEAR_BRIDGE_TOKEN to a nonempty ASCII token without whitespace")
	}
	settings, err := openCodeSettings(env("ECHOEAR_OPENCODE_AGENT", "echoear"), env("ECHOEAR_OPENCODE_MODEL", defaultOpenCodeModel))
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	data := env("XDG_DATA_HOME", filepath.Join(home, ".local/share"))
	state := env("XDG_STATE_HOME", filepath.Join(home, ".local/state"))
	local := filepath.Join(data, "echoear")
	ocURL, ocPassword := os.Getenv("ECHOEAR_OPENCODE_URL"), os.Getenv("ECHOEAR_OPENCODE_PASSWORD")
	ocAPI, err := openCodeUpstream(pooledClient(), state, ocURL, ocPassword)
	if err != nil {
		return err
	}
	pairPort, err := strconv.Atoi(env("ECHOEAR_OPENCODE_PAIR_PORT", "0"))
	if err != nil || pairPort < 0 || pairPort > 65535 || pairPort != 0 && (ocURL == "" || ocPassword == "") {
		return errors.New("ECHOEAR_OPENCODE_PAIR_PORT requires a valid port and a dedicated OpenCode URL and password")
	}
	ocDir := env("ECHOEAR_OPENCODE_DIR", filepath.Join(local, "opencode/workspace"))
	syscall.Umask(0o077)
	if !filepath.IsAbs(ocDir) {
		return errors.New("ECHOEAR_OPENCODE_DIR must be an absolute path")
	}
	if err := os.MkdirAll(ocDir, 0o700); err != nil {
		return err
	}
	workerPath, err := filepath.Abs(env("ECHOEAR_TTS_WORKER", "tts_worker.py"))
	if err != nil {
		return err
	}
	project := filepath.Dir(workerPath)
	if err := os.MkdirAll(local, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(local, 0o700); err != nil {
		return err
	}
	fallbackVoice := env("ECHOEAR_TTS_VOICE", heartVoice)
	db, err := openBridgeDB(context.Background(), env("ECHOEAR_DB_PATH", filepath.Join(local, "echoear.db")), local,
		os.Getenv("ECHOEAR_TURSO_URL"), os.Getenv("ECHOEAR_TURSO_AUTH_TOKEN"), fallbackVoice)
	if err != nil {
		return err
	}
	defer db.Close()
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
	if db.cloud != nil {
		done := make(chan struct{})
		go func() {
			defer close(done)
			db.syncPeriodically(root)
		}()
		defer func() { finish(); <-done }()
	}
	oc := newOpenCode(root, ocAPI, db, ocDir, settings)
	hex := hexClient{upstream{"Hex", pooledClient(), &fileCache[endpoint]{path: filepath.Join(data, "hex/local-api.json"), read: hexEndpoint}},
		&fileCache[string]{path: filepath.Join(data, "hex/settings.json"), read: hexQuery}}
	models := filepath.Join(local, "tts-models")
	model := env("ECHOEAR_TTS_MODEL", "kokoro-v1.0.onnx")
	if filepath.Base(model) == model {
		model = filepath.Join(models, model)
	}
	python := env("ECHOEAR_TTS_PYTHON", filepath.Join(home, ".local/share/echoear/tts-venv/bin/python"))
	worker := newSynthWorker(root, python, workerPath, model, filepath.Join(models, "voices-v1.0.bin"))
	b := &bridge{root: root, token: token, db: db, peers: peers{deviceStore(db)},
		hex: hex, oc: oc, pairPort: pairPort, worker: worker, chime: MakeIntercomChimePCM(), peerHTTP: pooledClient(),
		voices:  deviceVoiceStore(db, fallbackVoice),
		virtual: virtualStore(db), history: &chatHistory{db},
		ota:     otaFile{path: filepath.Join(env("ECHOEAR_OTA_DIR", filepath.Join(project, "../firmware/build")), "echoear.bin")},
		box3OTA: otaFile{path: filepath.Join(env("ECHOEAR_BOX3_OTA_DIR", filepath.Join(project, "../firmware/build-box3")), "echoear_box3.bin")}}
	b.ws = newWSHub(b)
	// Session history is a one-time, retryable backfill; the LAN service and
	// local history remain available while OpenCode is offline.
	historyDone := make(chan struct{})
	go func() {
		defer close(historyDone)
		for {
			ctx, cancel := context.WithTimeout(root, 45*time.Second)
			err := b.migrateSessionMessages(ctx)
			cancel()
			if err == nil || root.Err() != nil {
				return
			}
			log.Printf("EchoEar history backfill pending: %v", err)
			select {
			case <-root.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	defer func() { finish(); <-historyDone }()
	bin := findTunnelCLI(home)
	var runner tunnelRunner
	if bin != "" {
		runner = tunnelCLI(bin)
	}
	b.tunnels = newTunnelManager(b, bin, runner)
	b.tunnels.restore()
	b.timers = newTimerManager(b)
	if err := b.timers.restore(time.Now()); err != nil {
		b.timers.close()
		b.tunnels.shutdown()
		b.ws.shutdown()
		finish()
		_ = listener.Close()
		<-worker.done
		return err
	}
	go func() {
		ctx, cancel := context.WithTimeout(root, ttsWait)
		defer cancel()
		_, _ = b.clips.get(ctx, worker, b.voices.fallback, "Sent to everyone!")
		others, _ := b.peers.snapshot("")
		for _, p := range others {
			b.warmCharacter(ctx, b.voiceFor(p), p.Character)
		}
	}()
	server := &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 105 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Printf("EchoEar bridge listening on %s", listener.Addr())
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	schemaDone := make(chan struct{})
	go func() {
		defer close(schemaDone)
		if err := b.syncSchemaUpgrade(root); err != nil && root.Err() == nil {
			log.Printf("EchoEar MCP schema upgrade pending: %v", err)
		}
	}()
	defer func() { finish(); <-schemaDone }()
	select {
	case <-signals.Done():
		b.timers.close()
		b.tunnels.shutdown()
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
	b.timers.close()
	b.tunnels.shutdown()
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
	if len(os.Args) == 2 && os.Args[1] == "mcp" {
		if err := runMCPStdio(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
