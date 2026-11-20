package main

import (
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
)

var virtualID = regexp.MustCompile(`^virt-[0-9a-f]{8}$`)

type virtualClient struct {
	ID        string    `json:"id"`
	Character string    `json:"character"`
	Theme     string    `json:"theme"`
	Voice     string    `json:"voice,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func validLegacyVirtual(id string, v virtualClient) bool {
	_, theme := catPalettes[v.Theme]
	return id == v.ID && virtualID.MatchString(id) && characterRE.MatchString(v.Character) && theme && !v.CreatedAt.IsZero()
}

func (b *bridge) virtualSnapshot() (map[string]virtualClient, error) {
	if b.virtual == nil {
		return nil, nil
	}
	return b.virtual.all()
}

func (b *bridge) virtualByID(id string) (virtualClient, bool) {
	if b.virtual == nil {
		return virtualClient{}, false
	}
	v, ok, _ := b.virtual.get(id)
	return v, ok
}

func (b *bridge) virtualAPI(w http.ResponseWriter, r *http.Request, path string) (any, error) {
	if b.virtual == nil {
		return nil, problem(503, "Virtual clients unavailable")
	}
	switch {
	case path == "/api/virtual-clients" && r.Method == http.MethodGet:
		clients, err := b.virtualSnapshot()
		if err != nil {
			return nil, err
		}
		out := make([]object, 0, len(clients))
		for _, id := range slices.Sorted(maps.Keys(clients)) {
			out = append(out, b.publicVirtual(clients[id]))
		}
		return object{"virtual_clients": out}, nil
	case path == "/api/virtual-clients" && r.Method == http.MethodPost:
		obj, err := jsonBody(r)
		if err != nil {
			return nil, err
		}
		name, theme := strings.TrimSpace(str(obj["character"])), str(obj["theme"])
		if !characterRE.MatchString(name) {
			return nil, problem(400, "Invalid character name")
		}
		if _, ok := catPalettes[theme]; !ok {
			return nil, problem(400, "Expected theme cake, mint, plum, or sunny")
		}
		voice := str(obj["voice"])
		if _, supplied := obj["voice"]; supplied && !b.voices.allowed(voice) {
			return nil, problem(400, "Unsupported voice")
		}
		v := virtualClient{ID: "virt-" + randomHex(4), Character: name, Theme: theme, Voice: voice, CreatedAt: time.Now().UTC()}
		_, err = b.virtual.db.ExecContext(r.Context(), `INSERT INTO virtual_clients (id,character,theme,voice,created_at) VALUES (?,?,?,?,?)`, b.virtual.values(v.ID, v)...)
		if err != nil {
			return nil, problem(502, "Cannot create virtual client")
		}
		sendJSON(w, 201, b.publicVirtual(v))
		return nil, nil
	case strings.HasPrefix(path, "/api/virtual-clients/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/api/virtual-clients/")
		if b.tunnels != nil {
			b.tunnels.mu.Lock()
			defer b.tunnels.mu.Unlock()
		}
		if _, ok := b.virtualByID(id); !ok {
			return nil, problem(404, "Virtual client not found")
		}
		if b.tunnels != nil {
			if err := b.tunnels.removeLocked(r.Context(), id, false); err != nil {
				return nil, err
			}
		}
		err := b.virtual.save(id, virtualClient{}, true)
		if err != nil {
			return nil, err
		}
		if b.ws != nil {
			b.ws.disconnectDevice(id)
		}
		w.WriteHeader(204)
		return nil, nil
	}
	return nil, problem(404, "Not found")
}

func (b *bridge) publicVirtual(v virtualClient) object {
	connected := false
	if b.ws != nil {
		b.ws.mu.Lock()
		d := b.ws.devices[v.ID]
		connected = d != nil && b.ws.active(d)
		b.ws.mu.Unlock()
	}
	out := object{"id": v.ID, "character": v.Character, "theme": v.Theme, "created_at": v.CreatedAt,
		"online": connected, "ws_connected": connected, "voice": b.voices.get(v.ID, v.Theme)}
	if details := b.tunnels.info(v.ID); details != nil {
		out["tunnel"] = details
	}
	return out
}

func deviceTheme(character string) string {
	theme := strings.ToLower(character)
	if _, ok := catPalettes[theme]; ok {
		return theme
	}
	return "cake"
}

func (b *bridge) deviceList() ([]object, error) {
	others, err := b.peerSnapshot("")
	if err != nil {
		return nil, err
	}
	clients, err := b.virtualSnapshot()
	if err != nil {
		return nil, err
	}
	list := publicPeers(others)
	seen := make(map[string]bool, len(others))
	for _, entry := range list {
		seen[str(entry["device_id"])] = true
	}
	for id, v := range clients {
		if !seen[id] {
			list = append(list, object{"device_id": id, "character": v.Character, "online": false})
		}
	}
	for _, entry := range list {
		id := str(entry["device_id"])
		entry["kind"], entry["theme"] = "physical", deviceTheme(str(entry["character"]))
		if phoneID.MatchString(id) || virtualID.MatchString(id) {
			entry["kind"] = "virtual"
			delete(entry, "ip")
		}
		if v, ok := clients[id]; ok {
			entry["character"], entry["theme"] = v.Character, v.Theme
		}
		entry["voice"] = b.voices.get(id, str(entry["theme"]))
		if details := b.tunnels.info(id); details != nil {
			entry["tunnel"] = details
		}
		if b.ws != nil {
			b.ws.mu.Lock()
			b.ws.deviceStatus(entry, id)
			b.ws.mu.Unlock()
		}
		if entry["kind"] == "virtual" {
			entry["online"] = entry["ws_connected"] == true
		}
	}
	slices.SortFunc(list, func(a, b object) int { return strings.Compare(str(a["device_id"]), str(b["device_id"])) })
	return list, nil
}
