package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const ttsWait = 30 * time.Second

var errTTS = errors.New("TTS stream failed")

type ttsRequest struct {
	ctx         context.Context
	voice, text string
	done        chan ttsResult
}

type ttsResult struct {
	pcm []byte
	err error
}

type synthWorker struct {
	python, script, model, voices string
	ready                         atomic.Bool
	child                         atomic.Int64
	requests                      chan ttsRequest
	done                          chan struct{}
}

func (w *synthWorker) pid() int { return int(w.child.Load()) }

func newSynthWorker(ctx context.Context, python, script, model, voices string) *synthWorker {
	w := &synthWorker{python: python, script: script, model: model, voices: voices,
		requests: make(chan ttsRequest), done: make(chan struct{})}
	go w.run(ctx)
	return w
}

type workerProcess struct {
	cmd     *exec.Cmd
	in, out *os.File
	exited  chan struct{}
}

func (w *synthWorker) start() (*workerProcess, error) {
	cmd := exec.Command(w.python, "-B", w.script, w.model, w.voices)
	// Do not pass bridge tokens or upstream service credentials to the worker.
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"}
	in, stdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer in.Close()
	stdout, out, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	defer out.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, os.Stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	p := &workerProcess{cmd: cmd, in: stdin, out: stdout, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

func (p *workerProcess) stop() {
	p.in.Close()
	timer := time.NewTimer(time.Second)
	select {
	case <-p.exited:
		timer.Stop()
	case <-timer.C:
		_ = p.cmd.Process.Kill()
		<-p.exited
	}
	p.out.Close()
}

func ttsFrame(voice, text string) []byte {
	if voiceName(voice) == "" {
		voice = heartVoice
	}
	request := make([]byte, 0, 8+len(voice)+len(text))
	request = append(le.AppendUint32(request, uint32(len(voice))), voice...)
	return append(le.AppendUint32(request, uint32(len(text))), text...)
}

// call reports whether the process is unusable, distinct from an inference error.
func (p *workerProcess) call(ctx context.Context, voice, text string) (ttsResult, bool) {
	if err := ctx.Err(); err != nil {
		return ttsResult{err: err}, false
	}
	stop := context.AfterFunc(ctx, func() { _ = p.cmd.Process.Kill() })
	defer stop()
	if _, err := p.in.Write(ttsFrame(voice, text)); err != nil {
		return ttsResult{err: errTTS}, true
	}
	var header [9]byte
	if _, err := io.ReadFull(p.out, header[:]); err != nil {
		return ttsResult{err: errTTS}, true
	}
	rate, size := le.Uint32(header[1:]), le.Uint32(header[5:])
	status := header[0]
	if status > 1 || status == 1 && size > 4096 || size > maxPCM*3 {
		return ttsResult{err: errTTS}, true
	}
	if text == "" {
		if status != 0 || rate != 0 || size != 0 {
			return ttsResult{err: errTTS}, true
		}
		return ttsResult{}, false
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(p.out, raw); err != nil {
		return ttsResult{err: errTTS}, true
	}
	if status == 1 {
		return ttsResult{err: errTTS}, false
	}
	pcm, err := synthPCM(raw, rate)
	return ttsResult{pcm, err}, false
}

func (w *synthWorker) run(ctx context.Context) {
	defer close(w.done)
	backoff := 100 * time.Millisecond
	for ctx.Err() == nil {
		p, err := w.start()
		if err == nil {
			w.child.Store(int64(p.cmd.Process.Pid))
			timeout, cancel := context.WithTimeout(ctx, ttsWait)
			probe, _ := p.call(timeout, heartVoice, "")
			cancel()
			err = probe.err
			if err == nil {
				backoff = 100 * time.Millisecond
				w.ready.Store(true)
			serve:
				for {
					select {
					case <-ctx.Done():
						break serve
					case <-p.exited:
						break serve
					case req := <-w.requests:
						if err := req.ctx.Err(); err != nil {
							req.done <- ttsResult{err: err}
							continue
						}
						turn, cancel := context.WithTimeout(ctx, ttsWait)
						result, broken := p.call(turn, req.voice, req.text)
						cancel()
						req.done <- result
						if broken {
							break serve
						}
					}
				}
				w.ready.Store(false)
			}
			p.stop()
			w.child.Store(0)
		}
		if ctx.Err() != nil {
			return
		}
		log.Print("EchoEar TTS worker unavailable; restarting")
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
			backoff = min(5*time.Second, backoff*2)
		}
	}
}

func (w *synthWorker) synthesize(ctx context.Context, voice, text string) ([]byte, error) {
	req := ttsRequest{ctx, voice, text, make(chan ttsResult, 1)}
	select {
	case w.requests <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.done:
		return nil, errTTS
	}
	select {
	case res := <-req.done:
		return res.pcm, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type speechClips struct {
	mu    sync.Mutex
	clips map[clipKey]*clip
	order []clipKey
}

type clipKey struct{ voice, text string }

type clip struct {
	done chan struct{}
	pcm  []byte
	err  error
}

func (c *speechClips) peek(voice, text string) []byte {
	c.mu.Lock()
	item := c.clips[clipKey{voice, text}]
	c.mu.Unlock()
	if item != nil {
		select {
		case <-item.done:
			if item.err == nil {
				return item.pcm
			}
		default:
		}
	}
	return nil
}

func (c *speechClips) get(ctx context.Context, w *synthWorker, voice, text string) ([]byte, error) {
	key := clipKey{voice, text}
	c.mu.Lock()
	if c.clips == nil {
		c.clips = make(map[clipKey]*clip)
	}
	if existing := c.clips[key]; existing != nil {
		c.mu.Unlock()
		select {
		case <-existing.done:
			return existing.pcm, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	item := &clip{done: make(chan struct{})}
	if len(c.clips) == 32 {
		delete(c.clips, c.order[0])
		c.order = c.order[1:]
	}
	c.clips[key] = item
	c.order = append(c.order, key)
	c.mu.Unlock()
	item.pcm, item.err = w.synthesize(ctx, voice, text)
	if item.err != nil {
		c.mu.Lock()
		if c.clips[key] == item {
			delete(c.clips, key)
			for i, ordered := range c.order {
				if ordered == key {
					c.order = append(c.order[:i], c.order[i+1:]...)
					break
				}
			}
		}
		c.mu.Unlock()
	}
	close(item.done)
	return item.pcm, item.err
}

type AudioStream struct {
	cond    *sync.Cond
	chunks  [][]byte
	firstAt time.Time
	size    int
	done    bool
	err     error
	evict   func()
}

func newAudioStream() *AudioStream { return &AudioStream{cond: sync.NewCond(&sync.Mutex{})} }

func (s *AudioStream) finish(err error) {
	s.cond.L.Lock()
	evict := s.finishLocked(err)
	s.cond.L.Unlock()
	if evict != nil {
		evict()
	}
}

func (s *AudioStream) finishLocked(err error) func() {
	if s.done {
		return nil
	}
	s.done, s.err = true, err
	s.cond.Broadcast()
	if err != nil {
		s.chunks = nil
		return s.evict
	}
	return nil
}

func (s *AudioStream) append(pcm []byte) bool {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if s.done || len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > maxPCM-s.size {
		return false
	}
	if len(s.chunks) == 0 {
		s.firstAt = time.Now()
	}
	s.chunks = append(s.chunks, pcm)
	s.size += len(pcm)
	s.cond.Broadcast()
	return true
}

func (s *AudioStream) traceFirst(t *turnTrace, start time.Time) {
	if t == nil || start.IsZero() {
		return
	}
	s.cond.L.Lock()
	at := s.firstAt
	s.cond.L.Unlock()
	if !at.IsZero() {
		t.measure(diagTTSFirst, at.Sub(start))
	}
}
func (s *AudioStream) chunk(ctx context.Context, i int) ([]byte, error) {
	return s.waitChunk(ctx, i, ttsWait)
}

func (s *AudioStream) waitChunk(ctx context.Context, i int, wait time.Duration) ([]byte, error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if i >= len(s.chunks) && !s.done {
		timeout := time.AfterFunc(wait, func() { s.finish(errors.New("TTS stream timed out")) })
		defer timeout.Stop()
		wake := context.AfterFunc(ctx, func() {
			s.cond.L.Lock()
			s.cond.Broadcast()
			s.cond.L.Unlock()
		})
		defer wake()
		for i >= len(s.chunks) && !s.done && ctx.Err() == nil {
			s.cond.Wait()
		}
	}
	if s.err != nil {
		return nil, s.err
	}
	if i < len(s.chunks) {
		return s.chunks[i], nil
	}
	return nil, ctx.Err()
}

type audioEntry struct {
	id     string
	stream *AudioStream
}

type audioCache struct {
	mu    sync.Mutex
	order list.List
	items map[string]*list.Element
}

func (c *audioCache) add(s *AudioStream) (id string, size int, streaming bool) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()
	if s.err != nil || s.size == 0 {
		return "", 0, false
	}
	key := make([]byte, 18)
	_, _ = rand.Read(key)
	id = base64.RawURLEncoding.EncodeToString(key)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[string]*list.Element, 16)
	}
	c.items[id] = c.order.PushFront(audioEntry{id, s})
	s.evict = func() {
		c.mu.Lock()
		if e := c.items[id]; e != nil {
			c.order.Remove(e)
			delete(c.items, id)
		}
		c.mu.Unlock()
	}
	if c.order.Len() > 16 {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(audioEntry).id)
	}
	if s.done {
		return id, s.size, false
	}
	return id, 0, true
}

func (c *audioCache) ServeAudio(w http.ResponseWriter, r *http.Request, id string) error {
	c.mu.Lock()
	e := c.items[id]
	if e != nil {
		c.order.MoveToFront(e)
	}
	c.mu.Unlock()
	if e == nil {
		return problem(404, "Audio not found")
	}
	s := e.Value.(audioEntry).stream
	s.cond.L.Lock()
	err, done, size, chunks := s.err, s.done, s.size, s.chunks
	s.cond.L.Unlock()
	if err != nil {
		return problem(404, "Audio not found")
	}
	w.Header().Set("Content-Type", pcmType)
	if done {
		w.Header().Set("Content-Length", strconv.Itoa(size))
		for _, chunk := range chunks {
			if _, err := w.Write(chunk); err != nil {
				break
			}
		}
		return nil
	}
	flush := http.NewResponseController(w)
	w.WriteHeader(200)
	if flush.Flush() != nil {
		return nil
	}
	for i := 0; ; i++ {
		chunk, err := s.chunk(r.Context(), i)
		if err != nil {
			// Omit the terminating chunk; the firmware must see a failed transfer.
			panic(http.ErrAbortHandler)
		}
		if chunk == nil {
			return nil
		}
		if _, err := w.Write(chunk); err != nil || flush.Flush() != nil {
			return nil
		}
	}
}

func spokenText(text string) (string, error) {
	text = plainText(text)
	if runeLen(text) > maxText || !strings.ContainsFunc(text, alphaNum) {
		return "", problem(400, fmt.Sprintf("Expected spoken text of 1–%d characters", maxText))
	}
	return text, nil
}

type speechTurn struct {
	stream *AudioStream
	cancel context.CancelFunc
	queue  chan string
	sent   []string
	length int
}

func newSpeechTurn(ctx context.Context, cancel context.CancelFunc, w *synthWorker, voice string) *speechTurn {
	t := &speechTurn{stream: newAudioStream(), cancel: cancel, queue: make(chan string, maxText)}
	go func() {
		defer cancel()
		first := true
		for s := range t.queue {
			pcm, err := w.synthesize(ctx, voice, s)
			if err != nil {
				t.stream.finish(errTTS)
				return
			}
			if !first {
				pcm = append(make([]byte, sampleRate/5, sampleRate/5+len(pcm)), pcm...)
			}
			first = false
			if !t.stream.append(pcm) {
				t.stream.finish(errTTS)
				return
			}
		}
		t.stream.finish(nil)
	}()
	return t
}

func (t *speechTurn) send(s string) {
	// At most maxText printable characters can be accepted, so send never waits on TTS.
	n := runeLen(s)
	if n == 0 || t.length+n > maxText {
		return
	}
	t.length += n
	t.sent = append(t.sent, s)
	t.queue <- s
}

func (t *speechTurn) discard() {
	t.cancel()
	close(t.queue)
}

func (t *speechTurn) complete(final []string) bool {
	if len(t.sent) > len(final) {
		return false
	}
	for i, s := range t.sent {
		if s != final[i] {
			return false
		}
	}
	for _, s := range final[len(t.sent):] {
		t.send(s)
	}
	close(t.queue)
	return true
}
