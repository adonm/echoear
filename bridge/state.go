package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	deviceIDRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	sessionIDRE = regexp.MustCompile(`^ses[A-Za-z0-9_-]{1,128}$`)
	characterRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._'’-]{0,39}$`)
	directRE    = regexp.MustCompile(`(?i)^(?:hey\s+[\pL\pN_-]+,?\s+)?(?:tell|message|intercom|walkie|say\s+to|send)\s+`)
	composeRE   = regexp.MustCompile(`(?i)^(?:about|(?:(?:an?|the|some)\s+(?:[\pL\pN_'-]+\s+){0,5})?(?:joke|story|fact|riddle|quiz))(?:[^\pL\pN_]|$)`)
)

func runeLen(s string) int { return utf8.RuneCountInString(s) }

type store[T comparable] struct {
	path string
	mu   sync.Mutex
	data map[string]T // nil means invalid on disk; never replace it silently.
}

func loadStore[T comparable](path string, valid func(string, T) bool) *store[T] {
	s := &store[T]{path: path}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		s.data = map[string]T{}
	} else if err == nil && json.Unmarshal(raw, &s.data) == nil {
		for key, value := range s.data {
			if !deviceIDRE.MatchString(key) || !valid(key, value) {
				s.data = nil
				break
			}
		}
	} else {
		s.data = nil
	}
	return s
}

func (s *store[T]) invalid() error { return problem(502, "Invalid "+filepath.Base(s.path)) }

func writeMap(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".echoear-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// saveLocked rolls back the in-memory record if the replacement fails.
func (s *store[T]) saveLocked(key string, value T, remove bool) error {
	if s.data == nil {
		return s.invalid()
	}
	old, existed := s.data[key]
	if remove {
		if !existed {
			return nil
		}
		delete(s.data, key)
	} else {
		if existed && value == old {
			return nil
		}
		s.data[key] = value
	}
	if err := writeMap(s.path, s.data); err != nil {
		if existed {
			s.data[key] = old
		} else {
			delete(s.data, key)
		}
		return problem(502, fmt.Sprintf("Cannot save %s: %T", filepath.Base(s.path), err))
	}
	return nil
}

func lanIP(value string, loopback bool) (string, error) {
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "", problem(400, "Expected a numeric LAN IP address")
	}
	if ip.Is4In6() {
		return "", problem(400, "Peer address must be on the LAN")
	}
	if ip.IsPrivate() || (ip.Is4() && ip.IsLinkLocalUnicast()) || (loopback && ip.IsLoopback()) {
		return ip.String(), nil
	}
	return "", problem(400, "Peer address must be on the LAN")
}

func isHostIP(ip string) bool {
	if a, err := netip.ParseAddr(ip); err == nil && a.IsLoopback() {
		return true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		if host, _, ok := strings.Cut(addr.String(), "/"); ok && host == ip {
			return true
		}
	}
	return false
}

type peer struct {
	DeviceID  string `json:"device_id"`
	Character string `json:"character"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	LastSeen  int64  `json:"last_seen"`
}

func (p *peer) UnmarshalJSON(raw []byte) error {
	type fields peer
	v := struct {
		*fields
		LastSeen *int64 `json:"last_seen"`
	}{fields: (*fields)(p)}
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	if v.LastSeen == nil {
		return fmt.Errorf("missing last_seen")
	}
	p.LastSeen = *v.LastSeen
	return nil
}

type peers struct{ *store[peer] }

func loadPeers(path string) peers {
	return peers{loadStore(path, func(id string, p peer) bool {
		_, err := lanIP(p.IP, true)
		return id == p.DeviceID && err == nil && (characterRE.MatchString(p.Character) || p.Character == truncate(id, 40)) &&
			p.Port > 0 && p.Port <= 65535 && p.LastSeen >= 0
	})}
}

func (p peers) register(who peer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveLocked(who.DeviceID, who, false)
}

func (p peers) touch(id, ip, character string) (peer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data == nil {
		return peer{}, p.invalid()
	}
	who, known := p.data[id]
	who.DeviceID, who.LastSeen = id, time.Now().Unix()
	if character != "" {
		who.Character = character
	} else if !known {
		who.Character = truncate(id, 40)
	}
	if !known && !isHostIP(ip) {
		who.IP, who.Port = ip, 80
	}
	if who.IP != "" {
		return who, p.saveLocked(id, who, false)
	}
	return who, nil
}

func (p peers) snapshot(sender string) ([]peer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.data == nil {
		return nil, p.invalid()
	}
	others := make([]peer, 0, len(p.data))
	for id, who := range p.data {
		if !strings.EqualFold(sender, id) {
			others = append(others, who)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].DeviceID < others[j].DeviceID })
	return others, nil
}

func publicPeers(others []peer) []object {
	now := time.Now()
	out := make([]object, 0, len(others))
	for _, p := range others {
		out = append(out, object{"device_id": p.DeviceID, "character": p.Character, "ip": p.IP,
			"online": now.Sub(time.Unix(p.LastSeen, 0)) <= 180*time.Second})
	}
	return out
}

var broadcastNames = []string{"all", "everyone", "everybody"}

func broadcast(target string) bool {
	for _, name := range broadcastNames {
		if strings.EqualFold(target, name) {
			return true
		}
	}
	return false
}

func targets(others []peer, target string) []peer {
	if broadcast(target) {
		return others
	}
	var ids, characters []peer
	for _, p := range others {
		if strings.EqualFold(target, p.DeviceID) {
			ids = append(ids, p)
		} else if strings.EqualFold(target, p.Character) {
			characters = append(characters, p)
		}
	}
	if len(ids) != 0 {
		return ids
	}
	return characters
}

func directMessage(others []peer, text string) *intercomMessage {
	text = strings.TrimSpace(text)
	loc := directRE.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	text = text[loc[1]:]
	names := append(make([]string, 0, len(broadcastNames)+len(others)*2), broadcastNames...)
	for _, p := range others {
		names = append(names, p.Character, p.DeviceID)
	}
	sort.SliceStable(names, func(i, j int) bool { return runeLen(names[i]) > runeLen(names[j]) })
	for _, name := range names {
		head := truncate(text, runeLen(name))
		if !strings.EqualFold(name, head) || len(head) == len(text) {
			continue
		}
		tail := text[len(head):]
		r, width := utf8.DecodeRuneInString(tail)
		switch {
		case unicode.IsSpace(r) || r == ',' || r == ':':
		case strings.ContainsRune("-–—", r):
			next, _ := utf8.DecodeRuneInString(tail[width:])
			if !unicode.IsSpace(next) {
				continue
			}
		default:
			continue
		}
		tail = strings.TrimSpace(strings.TrimLeft(tail, " \t\r\n,:-–—"))
		if tail != "" && !composeRE.MatchString(tail) {
			return &intercomMessage{head, tail}
		}
		return nil
	}
	return nil
}

func turnPrompt(sender peer, others []peer, text string) string {
	names := make([]string, 0, min(16, len(others)))
	for _, who := range others[:min(16, len(others))] {
		names = append(names, who.Character+" ("+who.DeviceID+")")
	}
	list := "none"
	if len(names) != 0 {
		list = strings.Join(names, ", ")
	}
	return fmt.Sprintf("[EchoEar: you are %s (device_id=%s); other EchoEars: %s]\n%s", sender.Character, sender.DeviceID, list, text)
}
