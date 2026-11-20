package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCatMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "echoear.db")
	db := testDB(t, path, "")
	cats := []struct{ id, slug, name, bg, mark, accent, voice string }{
		{"echoear-33d8", "cake", "Cake", "#FFF3D1", "#F59E0B", "#FACC15", heartVoice},
		{"echoear-2cac", "mint", "Mint", "#7BE3C6", "#228E81", "#FFD166", fableVoice},
		{"virt-8458606f", "greg", "greg", "#FCD34D", "#EA580C", "#FFBB6E", heartVoice},
	}
	for _, c := range cats {
		if virtualID.MatchString(c.id) {
			if err := virtualStore(db).save(c.id, virtualClient{ID: c.id, Character: c.name, Theme: "sunny", CreatedAt: time.Now()}, false); err != nil {
				t.Fatal(err)
			}
		} else if err := deviceStore(db).save(c.id, peer{DeviceID: c.id, Character: c.name, IP: "192.168.1.9", Port: 80}, false); err != nil {
			t.Fatal(err)
		}
		if err := sessionStore(db).save(c.id, "ses_"+c.slug, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := voiceStore(db).save(cats[0].id, heartVoice, false); err != nil {
		t.Fatal(err)
	}
	if err := voiceStore(db).save(cats[2].id, fableVoice, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM meta WHERE key='migrated_cats_v1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDB(t, path, "")
	b := &bridge{db: db, peers: peers{deviceStore(db)}, virtual: virtualStore(db), voices: deviceVoiceStore(db, heartVoice)}
	for _, c := range cats {
		got, err := b.catTool(t.Context(), object{"op": "get", "device_id": c.id})
		want := catState{catLook: catLook{Name: c.name, Configured: true, FaceBG: c.bg, MarkColor: c.mark, AccentColor: c.accent}, Voice: c.voice, Extras: []object{}}
		active := want
		if c.slug == "greg" {
			active.Voice = fableVoice
		}
		if err != nil || !reflect.DeepEqual(got["cat"], active) {
			t.Fatalf("active %s: got %+v, want %+v: %v", c.name, got, active, err)
		}
		p, err := readSavedCat(db.QueryRow("SELECT "+savedCatColumns+" FROM saved_cats WHERE slug=?", c.slug))
		if err != nil || !reflect.DeepEqual(p.catState, want) {
			t.Fatalf("saved %s: got %+v, want %+v: %v", c.name, p, want, err)
		}
		if sid, ok, err := sessionStore(db).get(c.id); err != nil || !ok || sid != "ses_"+c.slug {
			t.Fatalf("session changed for %s: %s: %v", c.name, sid, err)
		}
	}
	if _, err := db.Exec("UPDATE cats SET name='Edited' WHERE device_id=?", cats[0].id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM saved_cats WHERE slug='mint'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = testDB(t, path, "")
	var name string
	var count int
	err := db.QueryRow("SELECT name,(SELECT count(*) FROM saved_cats) FROM cats WHERE device_id=?", cats[0].id).Scan(&name, &count)
	if err != nil || name != "Edited" || count != 2 {
		t.Fatalf("migration re-ran: %q %d %v", name, count, err)
	}
}

func TestCatToolAndLiveFace(t *testing.T) {
	title := make(chan string, 4)
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.voices = deviceVoiceStore(b.db, heartVoice)
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Path == "/api/session/ses_cat" {
				raw, _ := io.ReadAll(r.Body)
				title <- str(testObject(t, raw)["title"])
				fmt.Fprint(w, `{"data":{}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	post := func(path string, data object, status int) object {
		return testObject(t, testRequest(t, "POST", base+path, "unit", data, status))
	}
	created := post("/api/virtual-clients", object{}, 201)
	id := str(created["id"])
	if !virtualID.MatchString(id) || created["character"] != "New Cat" || created["cat"].(object)["configured"] != false {
		t.Fatal("virtual client did not start blank", created)
	}
	if err := b.oc.save(id, "ses_cat"); err != nil {
		t.Fatal(err)
	}
	call := func(fail bool, op string, args object) object {
		t.Helper()
		args["op"], args["device_id"] = op, id
		return toolTest(t, base, "cat", args, fail)
	}
	blank := call(false, "get", object{})["cat"].(object)
	if blank["name"] != "New Cat" || blank["face_bg"] != "#F3F4F6" || blank["mark_color"] != "#CBD5E1" ||
		blank["accent_color"] != "#60A5FA" || blank["voice"] != heartVoice || len(blank["extras"].([]any)) != 0 {
		t.Fatal("invalid blank starter", blank)
	}
	if got := turnPrompt(peer{DeviceID: id}, true, "voice", "Hi", blankCat(), turnContext{}); got != "[New Cat ("+id+") · web · voice · setup]\nHi" {
		t.Fatal(got)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.json(t, object{"ev": "hello", "role": "web", "device_id": id})
	f := dev.until(t, hasWS("svg", "Say hi to design me!"))
	if strings.Contains(string(f.data), "#F59E0B") {
		t.Fatal("starter face did not show its setup cue")
	}
	web := testConnectWS(t, base, "unit", true)
	web.json(t, object{"ev": "hello", "role": "web"})
	web.until(t, hasWS("peers", id))
	web.json(t, object{"ev": "watch", "device_id": id})
	web.until(t, hasWS("svg", "Say hi to design me!"))
	for _, fields := range []object{
		{"face_bg": "navy"}, {"voice": "unknown"}, {"name": strings.Repeat("x", 41)},
		{"personality": strings.Repeat("x", 161)}, {"configured": "false"},
		{"extras": make([]object, maxCatExtras+1)}, {"extras": []object{{"t": "btn"}}},
		{"extras": []object{{"t": "text", "x": 30, "c": "#123456"}}},
	} {
		call(true, "update", fields)
	}
	if got := call(false, "get", object{})["cat"]; !reflect.DeepEqual(got, blank) {
		t.Fatal("invalid update changed the stored starter", got)
	}
	extras := []object{
		{"id": "eye-l", "t": "text", "x": -50, "y": 500, "s": "Space 🚀\ncat", "c": "#abcdef", "on": "tap", "tap": "Don't"},
		{"t": "tri", "x1": 140, "y1": 70, "x2": 220, "y2": 70, "x3": 180, "y3": 42, "c": "#c0ffee"},
	}
	cosmo := call(false, "update", object{"name": "Cosmo", "face_bg": "#172344", "mark_color": "#abcdef",
		"accent_color": "#aefc36", "personality": "curious space explorer", "voice": fableVoice, "extras": extras})["cat"].(object)
	firstExtra := cosmo["extras"].([]any)[0].(object)
	if cosmo["configured"] != true || cosmo["mark_color"] != "#ABCDEF" || cosmo["voice"] != fableVoice ||
		firstExtra["id"] != "extra-0" || firstExtra["x"] != json.Number("0") || firstExtra["y"] != json.Number("359") ||
		firstExtra["s"] != "Space cat" || firstExtra["on"] != nil || firstExtra["tap"] != nil {
		t.Fatal("unexpected configured cat", cosmo)
	}
	expectValue(t, title, "EchoEar: Cosmo ("+id+")")
	frame := dev.until(t, hasWS("svg", "#172344"))
	if !hasWS("svg", "#C0FFEE")(frame) || !hasWS("svg", "#1A1926")(frame) {
		t.Fatal("live cat lost an extra or an animated eye")
	}
	web.until(t, hasWS("svg", "#172344"))
	web.until(t, hasWS("cat", `"character":"Cosmo"`))
	web.until(t, hasWS("saved_cats", ""))
	list := toolTest(t, base, "devices", object{}, false)["devices"].([]any)
	if len(list) != 1 || list[0].(object)["character"] != "Cosmo" || list[0].(object)["cat"].(object)["face_bg"] != "#172344" {
		t.Fatal("MCP devices did not reflect cat design", list)
	}
	profiles := toolTest(t, base, "cat", object{"op": "list_saved"}, false)["cats"].([]any)
	if len(profiles) != 1 || profiles[0].(object)["slug"] != "cosmo" || profiles[0].(object)["voice"] != fableVoice {
		t.Fatal("profile was not saved", profiles)
	}
	stored := b.catFor(peer{DeviceID: id})
	if got := turnPrompt(peer{DeviceID: id}, true, "voice", "Hi", stored, turnContext{}); got != "[Cosmo ("+id+") · web · voice · vibe: curious space explorer]\nHi" {
		t.Fatal(got)
	}
	if c := call(false, "reset_blank", object{})["cat"].(object); c["configured"] != false || c["personality"] != "" || len(c["extras"].([]any)) != 0 {
		t.Fatal("reset_blank left configured state", c)
	}
	expectValue(t, title, "EchoEar: New Cat ("+id+")")
	dev.until(t, hasWS("svg", "Say hi to design me!"))
	got := call(false, "load_saved", object{"slug": "cosmo"})["cat"]
	if !reflect.DeepEqual(cosmo, got) {
		t.Fatal("profile did not round-trip", got)
	}
	expectValue(t, title, "EchoEar: Cosmo ("+id+")")
	dev.until(t, hasWS("svg", "#172344"))
	catPath := "/api/devices/" + id + "/cat"
	if out := post(catPath, object{"personality": "stargazer"}, 200); out["cat"].(object)["personality"] != "stargazer" {
		t.Fatal(out)
	}
	toolTest(t, base, "cat", object{"op": "delete_saved", "slug": "cosmo"}, false)
	call(true, "load_saved", object{"slug": "cosmo"})
	post(catPath, object{"save_profile": true}, 200)
	post(catPath, object{"slug": "cosmo"}, 200)
	testRequest(t, "DELETE", base+"/api/cats/cosmo", "unit", nil, 204)
	if len(apiObjects(t, base+"/api/cats", "cats")) != 0 {
		t.Fatal("profile was not deleted")
	}
}

func TestCatResetArchivesAndClearsScene(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.virtual = virtualStore(b.db)
		b.voices = deviceVoiceStore(b.db, heartVoice)
		b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_reset/interrupt" {
				fmt.Fprint(w, `{"data":{"interrupted":false}}`)
				return
			}
			w.WriteHeader(404)
		})
	})
	v := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Keepsake", "theme": "mint"}, 201))
	id := str(v["id"])
	if err := b.oc.save(id, "ses_reset"); err != nil {
		t.Fatal(err)
	}
	dev := testConnectWS(t, base, "unit", false)
	dev.hello(t, id, "Ignored")
	toolTest(t, base, "scene", object{"device_id": id, "room": "party", "nodes": []object{{"id": "caption", "t": "text", "s": "Party"}}}, false)
	testRequest(t, "POST", base+"/api/reset", "unit", object{"device_id": id}, 200)
	dev.until(t, hasWS("svg", "Say hi to design me!"))
	s := toolTest(t, base, "snapshot", object{"device_id": id}, false)
	if s["scene"].(object)["custom_scene"] != false || s["device"].(object)["room"] != "" {
		t.Fatal("reset did not clear custom scene and room", s)
	}
	if sid, err := b.oc.get(id); err != nil || sid != "" {
		t.Fatal("reset did not clear active session mapping", sid, err)
	}
	profiles := apiObjects(t, base+"/api/cats", "cats")
	if len(profiles) != 1 || profiles[0].(object)["name"] != "Keepsake" || profiles[0].(object)["face_bg"] != "#7BE3C6" || profiles[0].(object)["voice"] != fableVoice {
		t.Fatal("reset did not archive cat", profiles)
	}
}

func TestCatExtrasStayOnAllPosesWithinFirmwareBudget(t *testing.T) {
	hat := object{"t": "rect", "x": 135, "y": 35, "w": 90, "h": 20, "c": "#AA7744", "fill": true}
	extras, err := sanitizeCatExtras(slices.Repeat([]any{hat}, maxCatExtras))
	if err != nil {
		t.Fatal(err)
	}
	for _, pose := range []catPose{
		{mood: "idle", battery: 100, charge: true},
		{mood: "idle", blink: true},
		{mood: "listening"},
		{mood: "thinking", dot: 2},
		{mood: "speaking", mouth: true, meter: true, battery: 20, charge: true},
		{mood: "sleepy"},
	} {
		cat := legacyCat("Test", "cake")
		cat.Extras = extras
		s := catScene(cat, pose)
		if len(s.nodes) > maxNodes {
			t.Fatal("scene exceeds firmware budget", pose)
		}
		for i := range extras {
			if nodeIn(s, fmt.Sprintf("extra-%d", i)) == nil {
				t.Fatal("pose removed an extra", pose, i)
			}
		}
	}
}

func TestCatReassign(t *testing.T) {
	for _, mode := range []string{"swap", "move"} {
		t.Run(mode, func(t *testing.T) {
			const fromID, fromSID, toSID = "echoear-from", "ses_from", "ses_to"
			interrupts := make(chan string, 2)
			titles := make(chan string, 2)
			b, base := wsHarness(t, func(b *bridge, dir string) {
				b.virtual = virtualStore(b.db)
				b.voices = deviceVoiceStore(b.db, heartVoice)
				b.oc = testOpenCode(t, b.root, dir, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/interrupt") {
						interrupts <- strings.TrimPrefix(r.URL.Path, "/api/session/")
						fmt.Fprint(w, `{"data":{"interrupted":true}}`)
					} else if r.Method == http.MethodPatch {
						raw, _ := io.ReadAll(r.Body)
						titles <- strings.TrimPrefix(r.URL.Path, "/api/session/") + ":" + str(testObject(t, raw)["title"])
						fmt.Fprint(w, `{"data":{}}`)
					} else {
						w.WriteHeader(404)
					}
				})
				b.oc.store = sessionStore(b.db)
			})
			post := func(path string, data object, status int) []byte {
				return testRequest(t, "POST", base+path, "unit", data, status)
			}
			target := testObject(t, post("/api/virtual-clients", object{"character": "Target", "theme": "mint", "voice": "bm_daniel"}, 201))
			toID := str(target["id"])
			from := testConnectWS(t, base, "unit", false)
			to := testConnectWS(t, base, "unit", false)
			from.hello(t, fromID, "Cake")
			to.hello(t, toID, "Ignored")
			source := post("/api/devices/"+fromID+"/cat", object{"name": "Source", "voice": "bf_emma", "face_bg": "#654321", "personality": "happy"}, 200)
			from.until(t, hasWS("scene", `"bg":"#654321"`))
			for id, sid := range map[string]string{fromID: fromSID, toID: toSID} {
				if err := b.oc.save(id, sid); err != nil {
					t.Fatal(err)
				}
			}
			canceled := make(chan string, 2)
			b.ws.mu.Lock()
			for _, id := range []string{fromID, toID} {
				d := b.ws.devices[id]
				d.busy = true
				d.cancel = func() { canceled <- id }
			}
			b.ws.mu.Unlock()

			const endpoint = "/api/cats/reassign"
			args := object{"from_device_id": fromID, "to_device_id": toID}
			testRequest(t, "POST", base+endpoint, "", args, 401)
			post(endpoint, object{"from_device_id": fromID, "to_device_id": fromID}, 400)
			post(endpoint, object{"from_device_id": fromID, "to_device_id": toID, "mode": "copy"}, 400)
			if mode == "move" {
				args["mode"] = mode
			} // Exercise the default swap.
			if got := testObject(t, post(endpoint, args, 200))["mode"]; got != mode {
				t.Fatal(got)
			}
			expectValue(t, interrupts, fromSID+"/interrupt")
			expectValue(t, interrupts, toSID+"/interrupt")
			expectValue(t, canceled, fromID)
			expectValue(t, canceled, toID)
			wantName, wantBG, wantVoice, wantSID := "Target", "#7BE3C6", "bm_daniel", toSID
			if mode == "move" {
				wantName, wantBG, wantVoice, wantSID = "New Cat", "#F3F4F6", heartVoice, ""
			} else {
				expectValue(t, titles, toSID+":EchoEar: Target ("+fromID+")")
			}
			expectValue(t, titles, fromSID+":EchoEar: Source ("+toID+")")
			from.until(t, hasWS("scene", `"bg":"`+wantBG+`"`))
			to.until(t, hasWS("svg", "#654321"))
			if gets := toolTest(t, base, "cat", object{"op": "get", "device_id": toID}, false)["cat"]; !reflect.DeepEqual(gets, testObject(t, source)["cat"]) {
				t.Fatal("target did not receive source design and voice", gets)
			}
			cat := toolTest(t, base, "cat", object{"op": "get", "device_id": fromID}, false)["cat"].(object)
			if cat["name"] != wantName || cat["face_bg"] != wantBG || cat["voice"] != wantVoice || cat["configured"] != (mode == "swap") {
				t.Fatal("incorrect source state after reassignment", cat)
			}
			for id, expected := range map[string]string{fromID: wantSID, toID: fromSID} {
				if sid, err := b.oc.get(id); err != nil || sid != expected {
					t.Fatalf("session mapping for %s = %s, want %s: %v", id, sid, expected, err)
				}
			}
			profiles := apiObjects(t, base+"/api/cats", "cats")
			i := slices.IndexFunc(profiles, func(v any) bool { return v.(object)["slug"] == "target" })
			if mode == "move" && (i < 0 || profiles[i].(object)["voice"] != "bm_daniel") || mode == "swap" && i >= 0 {
				t.Fatal("target archival is incorrect", profiles)
			}
		})
	}
}
