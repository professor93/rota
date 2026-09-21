package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestEveryGroupIsOnByDefault pins what a server built without an opinion
// answers: everything it answered before there was a way to ask for less.
func TestEveryGroupIsOnByDefault(t *testing.T) {
	h := newHarness(t, Options{})
	for _, path := range []string{"/", "/playground"} {
		if resp, _ := h.do("GET", path, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d", path, resp.StatusCode)
		}
	}
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/accounts: %d", resp.StatusCode)
	}
}

// TestPlaygroundOffIsAPageThatWasNeverThere: a group that is off is not
// registered, so it answers 404 like any path this server never had. The
// API it would have called is untouched.
func TestPlaygroundOffIsAPageThatWasNeverThere(t *testing.T) {
	h := newHarness(t, Options{Routes: &Routes{API: true, WebSocket: true}})
	for _, path := range []string{"/", "/playground"} {
		if resp, _ := h.do("GET", path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s answered %d, want 404", path, resp.StatusCode)
		}
	}
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the API must be untouched: %d", resp.StatusCode)
	}
}

func TestWebSocketOffIsThreeRoutesThatWereNeverThere(t *testing.T) {
	h := newHarness(t, Options{Routes: &Routes{API: true, Playground: true}})
	for _, path := range []string{"/v1/ws", "/v1/accounts/1/ws", "/v1/runs/r-1/ws"} {
		if resp, _ := h.do("GET", path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s answered %d, want 404", path, resp.StatusCode)
		}
	}
	if resp, _ := h.do("GET", "/v1/accounts", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("the API must be untouched: %d", resp.StatusCode)
	}
	// And the page is told, so it does not offer a run it cannot hold.
	_, raw := h.do("GET", "/v1/schema", nil)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	if doc["websocket"] != false {
		t.Fatalf("the schema must say the sockets are off: %s", raw)
	}
}

func TestAPIOffLeavesNothing(t *testing.T) {
	h := newHarness(t, Options{Routes: &Routes{}})
	for _, path := range []string{"/", "/playground", "/v1/schema", "/v1/accounts", "/v1/ws"} {
		if resp, _ := h.do("GET", path, nil); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s answered %d, want 404", path, resp.StatusCode)
		}
	}
}
