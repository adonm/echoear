package main

import (
	"bytes"
	"testing"
)

func TestParentPage(t *testing.T) {
	_, base := wsHarness(t, nil)
	page := testRequest(t, "GET", base+"/app/parent", "", nil, 200)
	for _, field := range []string{"Parent access", "Quiet hours and daily limit", "Activity in the last 7 days", "Open this cat's chat history"} {
		if !bytes.Contains(page, []byte(field)) {
			t.Errorf("parent page is missing %q", field)
		}
	}
	testRequest(t, "GET", base+"/app/parent.js", "", nil, 200)
}
