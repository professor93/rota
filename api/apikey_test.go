package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// Made-up credentials in the shapes Claude Code tells apart. None of them is
// anybody's.
const (
	testKey   = "sk-ant-api03-TESTONLY-not-a-real-key-0000000000-WXYZ"
	testSetup = "sk-ant-oat01-TESTONLY-not-a-real-setup-token-2222"
)

// The begin route takes a pasted credential in its body and finishes at once:
// a key makes an account outside the rotation, a setup token goes on the
// account named. Nothing that comes back, or is listed afterwards, carries
// either.
func TestPastedCredentialsOverHTTP(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer api.Close()
	old := rota.ClaudeModelsEndpoint
	rota.ClaudeModelsEndpoint = api.URL
	defer func() { rota.ClaudeModelsEndpoint = old }()

	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	clean := func(raw []byte) {
		t.Helper()
		if strings.Contains(string(raw), "TESTONLY") {
			t.Fatalf("a credential came back: %s", raw)
		}
	}

	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "claude", "key": testKey, "label": "work"})
	clean(raw)
	var got struct {
		ID         int    `json:"id"`
		Status     string `json:"status"`
		Credential string `json:"credential"`
		Order      int    `json:"order"`
		Label      string `json:"label"`
	}
	json.Unmarshal(raw, &got)
	if resp.StatusCode != 200 || got.Status != "added" || got.Credential != "apikey" || got.Order != 0 || got.Label != "work" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	resp, raw = h.do("GET", "/v1/accounts", nil)
	clean(raw)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"credential":"apikey"`) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	keyed := strconv.Itoa(got.ID)
	if resp, raw := h.do("PATCH", "/v1/accounts/"+keyed, map[string]any{"remote_control": true}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "API key") {
		t.Fatalf("remote control refuses a key: %d %s", resp.StatusCode, raw)
	}

	resp, raw = h.do("POST", "/v1/login", map[string]any{"account": 1, "token": testSetup})
	clean(raw)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"status":"long"`) {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}

	for _, body := range []map[string]any{
		{"key": testSetup},                      // the wrong kind
		{"account": 1, "token": testKey},        // the wrong kind the other way
		{"token": testSetup},                    // no account named
		{"key": testKey, "long": true},          // two logins at once
		{"key": testKey, "token": testSetup},    // two credentials at once
		{"account": got.ID, "token": testSetup}, // a key account takes no setup token
		{"provider": "claude", "label": "x"},    // a label without a key
	} {
		resp, raw := h.do("POST", "/v1/login", body)
		clean(raw)
		if resp.StatusCode != 400 {
			t.Fatalf("%v: %d %s", body, resp.StatusCode, raw)
		}
	}
}
