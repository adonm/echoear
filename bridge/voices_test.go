package main

import (
	"bytes"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTTSVoiceFrame(t *testing.T) {
	for _, tc := range []struct{ voice, encoded string }{
		{fableVoice, fableVoice},
		{heartVoice, heartVoice},
		{"unsupported", heartVoice},
		{"", heartVoice},
	} {
		const text = "Café"
		want := []byte(string(rune(len(tc.encoded))) + "\x00\x00\x00" + tc.encoded + "\x05\x00\x00\x00" + text)
		if got := ttsFrame(tc.voice, text); !bytes.Equal(got, want) {
			t.Errorf("ttsFrame(%q) = %q, want %q", tc.voice, got, want)
		}
	}
}

func TestSpeechClipsCacheByVoice(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int32
	w := fakeTTS(ctx, &calls)
	var clips speechClips
	for _, voice := range []string{emmaVoice, fableVoice, emmaVoice, fableVoice} {
		if pcm, err := clips.get(ctx, w, voice, "Hello!"); err != nil || len(pcm) == 0 {
			t.Fatalf("voice %s: no PCM (%v)", voice, err)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("synthesized %d clips, want one per voice", n)
	}
}

func TestDeviceVoiceDefaults(t *testing.T) {
	b, _ := wsHarness(t, func(b *bridge, dir string) {
		b.voices = deviceVoiceStore(b.db, "")
		b.virtual = virtualStore(b.db)
		if err := b.virtual.save("virt-01234567", virtualClient{ID: "virt-01234567", Character: "Green Cat", Theme: "mint"}, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, tc := range []struct {
		device, character string
		want              string
	}{
		{"echoear-33d8", "Cake", heartVoice},
		{"ECHOEAR-2CAC", "Other", heartVoice},
		{"echoear-other", "Mint", fableVoice},
		{"virt-01234567", "Green Cat", fableVoice},
		{"virt-1234abcd", "Greg", heartVoice},
	} {
		if got := b.voiceFor(peer{DeviceID: tc.device, Character: tc.character}); got != tc.want {
			t.Errorf("%s: got voice %q, want %q", tc.device, got, tc.want)
		}
	}
	configured := deviceVoiceStore(b.db, emmaVoice)
	if !configured.allowed(heartVoice) ||
		configured.get("echoear-33d8", "cake") != emmaVoice ||
		configured.get("echoear-2cac", "mint") != fableVoice {
		t.Fatal("explicit fallback voice or the Mint default was not respected")
	}
}

func TestDeviceVoiceAPI(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.voices = deviceVoiceStore(b.db, emmaVoice)
		b.virtual = virtualStore(b.db)
	})
	cake := testConnectWS(t, base, "unit", false)
	cake.hello(t, "echoear-33d8", "Cake")
	profile := testObject(t, testRequest(t, "GET", base+"/api/scope", "unit", nil, 200))
	if profile["scoped"] != false {
		t.Fatal("LAN listener reported itself as scoped")
	}
	path := "/api/devices/ECHOEAR-33D8/voice"
	testRequest(t, "POST", base+path, "", object{"voice": fableVoice}, 401)
	for _, voice := range []any{"bad", 1, nil} {
		testRequest(t, "POST", base+path, "unit", object{"voice": voice}, 400)
	}
	testRequest(t, "POST", base+path, "unit", object{"voice": heartVoice}, 200)
	got := testObject(t, testRequest(t, "POST", base+path, "unit", object{"voice": fableVoice}, 200))
	if got["device_id"] != "ECHOEAR-33D8" || got["voice"] != fableVoice {
		t.Fatalf("unexpected update response: %v", got)
	}
	if voice, _, err := voiceStore(b.db).get("echoear-33d8"); err != nil || voice != fableVoice {
		t.Fatal("override not persisted under lowercase device ID")
	}
	if d := apiObjects(t, base+"/api/devices", "devices")[0].(object); d["voice"] != fableVoice {
		t.Fatal("LAN device list omitted the override")
	}
	testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{"character": "Green", "theme": "mint", "voice": "unknown"}, 400)
	created := testObject(t, testRequest(t, "POST", base+"/api/virtual-clients", "unit", object{
		"character": "Green", "theme": "mint", "voice": "bm_george",
	}, 201))
	id := str(created["id"])
	if listed := apiObjects(t, base+"/api/virtual-clients", "virtual_clients"); len(listed) != 1 ||
		created["voice"] != "bm_george" || listed[0].(object)["voice"] != "bm_george" {
		t.Fatal("virtual client did not persist its initial voice")
	}
	v, ok := b.virtualByID(id)
	if !ok {
		t.Fatal("virtual client not found")
	}
	scoped := httptest.NewServer(b.scoped(v, "scoped-test"))
	defer scoped.Close()
	path = "/api/devices/" + id + "/voice"
	testRequest(t, "POST", scoped.URL+path, "unit", object{"voice": emmaVoice}, 401)
	testRequest(t, "POST", scoped.URL+"/api/devices/echoear-33d8/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "POST", scoped.URL+"/api/devices/virt-1234abcd/voice", "scoped-test", object{"voice": emmaVoice}, 403)
	testRequest(t, "GET", scoped.URL+path, "scoped-test", nil, 404)
	testRequest(t, "POST", scoped.URL+path, "scoped-test", object{"voice": heartVoice}, 200)
	scope := testObject(t, testRequest(t, "GET", scoped.URL+"/api/scope", "scoped-test", nil, 200))
	if scope["voice"] != heartVoice || !reflect.DeepEqual(profile["voices"], scope["voices"]) || len(scope["voices"].([]any)) != len(voiceCatalog) || b.voices.get("echoear-33d8", "cake") != fableVoice {
		t.Fatal("scoped update did not stay on the scoped cat")
	}
}

func TestMenuVoicePicker(t *testing.T) {
	b, base := wsHarness(t, func(b *bridge, dir string) {
		b.voices = deviceVoiceStore(b.db, heartVoice)
	})
	const id = "echoear-33d8"
	c := testConnectWS(t, base, "unit", false)
	c.hello(t, id, "Cake")
	web := testConnectWS(t, base, "unit", true)
	web.watch(t, id)
	web.json(t, object{"ev": "tap", "id": "menu"})
	web.until(t, hasWS("svg", "Heart"))
	web.json(t, object{"ev": "tap", "id": "menu-voice"})
	web.until(t, hasWS("svg", "voice-george"))
	for _, voice := range []int{1, 2, 3, 0} {
		choice := voiceCatalog[voice]
		web.json(t, object{"ev": "tap", "id": "voice-" + strings.ToLower(choice.Name)})
		web.until(t, hasWS("svg", "#FFD68B"))
		if reloaded := deviceVoiceStore(b.db, heartVoice); reloaded.get(id, "cake") != choice.ID {
			t.Errorf("voice picker did not persist %s", choice.ID)
		}
	}
	web.json(t, object{"ev": "tap", "id": "menu-back"})
	web.until(t, hasWS("svg", "menu-voice"))
	testRequest(t, "POST", base+"/api/devices/"+id+"/voice", "unit", object{"voice": "bf_alice"}, 200)
	web.until(t, hasWS("svg", "Alice"))
}
