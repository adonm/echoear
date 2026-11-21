package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type hexStream struct {
	api         upstream
	path, owner string
	partials    bool
	lease       time.Duration
}

func (h hexClient) openStream(ctx context.Context) (*hexStream, error) {
	var result struct {
		ID         uint64
		OwnerToken string
		Partials   bool
		LeaseMs    int64
	}
	input := object{"sampleFormat": "s16le", "sampleRate": sampleRate}
	// Never switch the selected model. If the live engine has different
	// settings, Hex rejects this stream and we use the established clip API.
	if h.query != nil {
		query, err := h.query.get(false)
		if err != nil {
			return nil, h.invalid()
		}
		settings, err := url.ParseQuery(query)
		if err != nil {
			return nil, h.invalid()
		}
		for _, key := range []string{"model", "language"} {
			if value := settings.Get(key); value != "" {
				input[key] = value
			}
		}
	}
	err := h.jsonCall(ctx, http.MethodPost, "/streams", input, &result)
	if err != nil {
		return nil, err
	}
	if result.OwnerToken == "" || result.LeaseMs <= 0 || result.LeaseMs > 60000 {
		return nil, h.invalid()
	}
	dst, err := h.config.get(false)
	if err != nil {
		return nil, h.unavailable(err)
	}
	return &hexStream{upstream{h.name, h.client, fixedEndpoint(dst)}, fmt.Sprintf("/streams/%d", result.ID),
		result.OwnerToken, result.Partials, time.Duration(result.LeaseMs) * time.Millisecond}, nil
}

func (s *hexStream) request(ctx context.Context, method, action string, pcm []byte) (*http.Response, error) {
	dst, _ := s.api.config.get(false)
	var body io.Reader
	if len(pcm) > 0 {
		body = bytes.NewReader(pcm)
	}
	req, err := http.NewRequestWithContext(ctx, method, dst.base+s.path+"/"+action, body)
	if err != nil {
		return nil, s.api.unavailable(err)
	}
	req.Header.Set("Authorization", dst.auth)
	req.Header.Set("X-Hex-Stream-Token", s.owner)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if action == "events" {
		req.Header.Set("Accept", "text/event-stream")
	}
	res, err := s.api.client.Do(req)
	if err != nil {
		return nil, s.api.unavailable(err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, &upstreamStatus{problem(502, fmt.Sprintf("Hex stream returned HTTP %d", res.StatusCode)), res.StatusCode}
	}
	return res, nil
}

func (s *hexStream) post(ctx context.Context, action string, pcm []byte) error {
	res, err := s.request(ctx, http.MethodPost, action, pcm)
	if err == nil {
		res.Body.Close()
	}
	return err
}

func (s *hexStream) cancel() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.post(ctx, "cancel", nil)
}

func (s *hexStream) finish(ctx context.Context) (string, error) {
	res, err := s.request(ctx, http.MethodPost, "finish", nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct{ Text *string }
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out) != nil || out.Text == nil {
		return "", s.api.invalid()
	}
	return strings.TrimSpace(*out.Text), nil
}

type hexStreamEvent struct {
	Type, Stable, Tentative, Text string
	Segment                       int
}

func (s *hexStream) events(ctx context.Context, handle func(hexStreamEvent)) error {
	res, err := s.request(ctx, http.MethodGet, "events", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return s.api.invalid()
	}
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 1<<20)
	for scan.Scan() {
		raw, ok := bytes.CutPrefix(scan.Bytes(), []byte("data:"))
		if !ok {
			continue
		}
		var event hexStreamEvent
		if json.Unmarshal(raw, &event) != nil {
			return s.api.invalid()
		}
		handle(event)
		switch event.Type {
		case "final":
			return nil
		case "cancelled", "error":
			return errors.New("Hex stream ended early")
		}
	}
	return io.ErrUnexpectedEOF
}
