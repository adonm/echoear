package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
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

const (
	emmaVoice  = "bf_emma"
	fableVoice = "bm_fable"
	heartVoice = "af_heart"
)

// The first four voices fit on the device picker; the browser offers all of them.
var voiceCatalog = [...]struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}{
	{heartVoice, "Heart"}, {fableVoice, "Fable"}, {emmaVoice, "Emma"}, {"bm_george", "George"},
	{"bf_alice", "Alice"}, {"bf_isabella", "Isabella"}, {"bf_lily", "Lily"}, {"bm_daniel", "Daniel"}, {"bm_lewis", "Lewis"},
}

func voiceName(id string) string {
	for _, voice := range voiceCatalog {
		if voice.ID == id {
			return voice.Name
		}
	}
	return ""
}

type deviceVoices struct {
	*store[string]
	fallback string
}

func deviceVoiceStore(db *bridgeDB, fallback string) deviceVoices {
	if voiceName(fallback) == "" {
		fallback = heartVoice
	}
	return deviceVoices{voiceStore(db), fallback}
}

func (v deviceVoices) allowed(voice string) bool {
	return voiceName(voice) != ""
}

func (v deviceVoices) defaultFor(theme string) string {
	if theme == "mint" {
		return fableVoice
	}
	if v.fallback != "" {
		return v.fallback
	}
	return heartVoice
}

func (v deviceVoices) get(id, theme string) string {
	id = strings.ToLower(id)
	if v.store != nil {
		var voice string
		_ = v.db.QueryRow("SELECT "+catVoiceSQL, id, id).Scan(&voice)
		if v.allowed(voice) {
			return voice
		}
	}
	return v.defaultFor(theme)
}

func (v deviceVoices) save(id, voice string) error {
	if v.store == nil {
		return problem(503, "Voice settings unavailable")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.store.save(strings.ToLower(id), voice, voice == "")
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

func validLegacyPeer(id string, p peer) bool {
	_, err := lanIP(p.IP, true)
	return id == p.DeviceID && err == nil && (characterRE.MatchString(p.Character) || p.Character == truncate(id, 40)) &&
		p.Port > 0 && p.Port <= 65535 && p.LastSeen >= 0
}

func (p peers) register(who peer) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.save(who.DeviceID, who, false)
}

func (p peers) snapshot(sender string) ([]peer, error) {
	data, err := p.all()
	if err != nil {
		return nil, err
	}
	others := make([]peer, 0, len(data))
	for id, who := range data {
		if !strings.EqualFold(sender, id) {
			others = append(others, who)
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].DeviceID < others[j].DeviceID })
	return others, nil
}

func publicPeers(others []peer) []object {
	out := make([]object, 0, len(others))
	for _, p := range others {
		out = append(out, object{"device_id": p.DeviceID, "character": p.Character, "ip": p.IP,
			"port": p.Port, "last_seen": p.LastSeen, "online": false})
	}
	return out
}

type intercomMessage struct{ target, text string }

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
