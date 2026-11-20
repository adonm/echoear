package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

func TestSessionMatchesDevice(t *testing.T) {
	for _, tc := range []struct {
		title, device string
		want          bool
	}{
		{"EchoEar: echoear-33d8", "echoear-33d8", true},
		{"EchoEar: scope-virt-1234", "virt-1234", true},
		{"EchoEar: Cake (echoear-33d8)", "ECHOEAR-33D8", true},
		{"EchoEar: echoear-33d80", "echoear-33d8", false},
		{"EchoEar: scope-echoear-33d80", "echoear-33d8", false},
		{"EchoEar: Cake (echoear-33d80)", "echoear-33d8", false},
		{"EchoEar: Cake (echoear-33d8) archived", "echoear-33d8", false},
		{"EchoEar: Cake(echoear-33d8)", "echoear-33d8", false},
		{"Other: Cake (echoear-33d8)", "echoear-33d8", false},
	} {
		if got := sessionMatchesDevice(tc.title, tc.device); got != tc.want {
			t.Errorf("sessionMatchesDevice(%q, %q) = %t, want %t", tc.title, tc.device, got, tc.want)
		}
	}
}

func TestListDeviceSessionsFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		activeSID string
		want      []string
		wantError bool
	}{
		{"unlisted active", http.StatusOK, "ses_active", []string{"ses_old", "ses_active"}, false},
		{"no active", http.StatusOK, "", []string{"ses_old"}, false},
		{"unavailable with active", http.StatusServiceUnavailable, "ses_active", []string{"ses_active"}, false},
		{"unavailable without active", http.StatusServiceUnavailable, "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := historyOpenCode(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/session" {
					t.Error("unexpected path", r.URL.Path)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"data":[{"id":"ses_old","title":"EchoEar: echoear-history","time":{"created":100}}]}`)
			})
			got, err := o.ListDeviceSessions(t.Context(), "echoear-history", tc.activeSID)
			if !slices.Equal(got, tc.want) || (err != nil) != tc.wantError {
				t.Fatalf("ListDeviceSessions = %v, %v; want %v (error: %t)", got, err, tc.want, tc.wantError)
			}
		})
	}
}

func TestSessionMessageBackfillExceedsOldPaginationLimit(t *testing.T) {
	o := historyOpenCode(t, func(w http.ResponseWriter, r *http.Request) {
		offset := 0
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			var err error
			offset, err = strconv.Atoi(cursor)
			if err != nil || offset > 500 {
				t.Error("unexpected cursor", cursor)
				http.Error(w, "unexpected cursor", 400)
				return
			}
		}
		data := make([]object, 50)
		for i := range data {
			data[i] = object{"id": fmt.Sprintf("msg_%d", offset+i), "type": "user",
				"text": "Example", "time": object{"created": offset + i}}
		}
		next := ""
		if offset < 500 {
			next = strconv.Itoa(offset + len(data))
		}
		_ = json.NewEncoder(w).Encode(object{"data": data, "cursor": object{"next": next}})
	})
	messages, err := o.GetSessionMessages(t.Context(), "ses_test")
	if err != nil || len(messages) != 550 || messages[0].ID != "msg_0" || messages[549].ID != "msg_549" {
		t.Fatalf("expected all 550 session messages, got %d (%v)", len(messages), err)
	}
}

func historyOpenCode(t *testing.T, h http.HandlerFunc) *openCode {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	path := filepath.Join(t.TempDir(), "service.json")
	if err := writeMap(path, object{"url": server.URL, "password": "unit"}); err != nil {
		t.Fatal(err)
	}
	return &openCode{upstream: upstream{"OpenCode", server.Client(), &fileCache[endpoint]{path: path, read: openCodeEndpoint}}}
}
