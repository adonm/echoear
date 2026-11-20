package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func pooledClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type endpoint struct{ base, auth string }

type fileCache[T any] struct {
	path  string
	read  func([]byte) (T, error)
	mu    sync.Mutex
	info  os.FileInfo
	value T
}

func (c *fileCache[T]) get(force bool) (value T, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	info, err := os.Stat(c.path)
	if err != nil {
		return value, err
	}
	if !force && c.info != nil && info.ModTime().Equal(c.info.ModTime()) && info.Size() == c.info.Size() && os.SameFile(info, c.info) {
		return c.value, nil
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return value, err
	}
	value, err = c.read(raw)
	if err == nil {
		c.info, c.value = info, value
	}
	return value, err
}

type upstream struct {
	name   string
	client *http.Client
	config *fileCache[endpoint]
}

func hexEndpoint(raw []byte) (endpoint, error) {
	var info struct {
		Port  int    `json:"port"`
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Port < 1 || info.Port > 65535 || info.Token == "" {
		return endpoint{}, errors.New("invalid Hex service configuration")
	}
	return endpoint{fmt.Sprintf("http://127.0.0.1:%d", info.Port), "Bearer " + info.Token}, nil
}

func openCodeEndpoint(raw []byte) (endpoint, error) {
	var info struct{ URL, Password string }
	if json.Unmarshal(raw, &info) != nil || info.Password == "" {
		return endpoint{}, errors.New("invalid OpenCode service configuration")
	}
	u, err := url.Parse(info.URL)
	if err != nil || u.Scheme != "http" {
		return endpoint{}, errors.New("invalid OpenCode service URL")
	}
	host := "127.0.0.1"
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "0.0.0.0":
	case "::", "::1":
		host = "::1"
	default:
		return endpoint{}, errors.New("not a local OpenCode service URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return endpoint{}, errors.New("invalid OpenCode service port")
	}
	return endpoint{"http://" + net.JoinHostPort(host, u.Port()),
		"Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:"+info.Password))}, nil
}

type upstreamStatus struct {
	*apiError
	code int
}

func (u upstream) unavailable(err error) *apiError {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return problem(504, u.name+" timed out")
	}
	return problem(502, u.name+" unavailable or returned invalid data")
}

func (u upstream) invalid() *apiError { return problem(502, u.name+" returned invalid data") }

func (u upstream) request(ctx context.Context, method, path, kind, accept string, size int64, body func() io.Reader) (*http.Response, error) {
	for attempt := range 2 {
		dst, err := u.config.get(attempt != 0)
		if err != nil {
			return nil, u.unavailable(err)
		}
		var payload io.Reader
		if body != nil {
			payload = body()
		}
		req, err := http.NewRequestWithContext(ctx, method, dst.base+path, payload)
		if err != nil {
			return nil, u.unavailable(err)
		}
		req.Header.Set("Authorization", dst.auth)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if body != nil {
			req.Header.Set("Content-Type", kind)
			req.ContentLength = size
		}
		res, err := u.client.Do(req)
		if err != nil {
			return nil, u.unavailable(err)
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return res, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 16<<10))
		res.Body.Close()
		if res.StatusCode != 401 || attempt == 1 {
			return nil, &upstreamStatus{problem(502, fmt.Sprintf("%s returned HTTP %d", u.name, res.StatusCode)), res.StatusCode}
		}
	}
	panic("unreachable")
}

func (u upstream) json(ctx context.Context, method, path, kind string, size int64, body func() io.Reader, out any) error {
	res, err := u.request(ctx, method, path, kind, "", size, body)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	const limit = 4 << 20
	raw, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil || len(raw) > limit || (len(raw) != 0 && !json.Valid(raw)) {
		return u.unavailable(err)
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return u.invalid()
	}
	return nil
}

func (u upstream) jsonCall(ctx context.Context, method, path string, in, out any) error {
	if in == nil {
		return u.json(ctx, method, path, "", 0, nil, out)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return u.invalid()
	}
	return u.json(ctx, method, path, jsonType, int64(len(raw)), func() io.Reader { return bytes.NewReader(raw) }, out)
}

func (u upstream) reachable(ctx context.Context, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	return u.jsonCall(ctx, http.MethodGet, path, nil, nil) == nil
}

type hexClient struct {
	upstream
	query *fileCache[string]
}

func hexQuery(raw []byte) (string, error) {
	settings := struct {
		Transcription struct{ Model, Language string }
	}{}
	settings.Transcription.Language = "en"
	if json.Unmarshal(raw, &settings) != nil || settings.Transcription.Model == "" {
		return "", errors.New("invalid Hex transcription settings")
	}
	t := settings.Transcription
	return url.Values{"model": {t.Model}, "language": {t.Language}}.Encode(), nil
}

func (h hexClient) transcribe(ctx context.Context, audio []byte, kind string) (string, error) {
	query, err := h.query.get(false)
	if err != nil {
		return "", h.invalid()
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	size := len(audio)
	body := func() io.Reader { return bytes.NewReader(audio) }
	if kind == pcmType {
		header := wavHeader(size)
		size += len(header)
		body = func() io.Reader { return io.MultiReader(bytes.NewReader(header[:]), bytes.NewReader(audio)) }
	}
	var result struct{ Transcript *string }
	if err := h.json(ctx, http.MethodPost, "/transcriptions?"+query, wavType, int64(size), body, &result); err != nil {
		return "", err
	}
	if result.Transcript == nil {
		return "", h.invalid()
	}
	return strings.TrimSpace(*result.Transcript), nil
}
