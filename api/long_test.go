package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// The HTTP surface offers the long login the same way the command line does:
// one flag on the begin, the same finish, and an answer that carries the
// date and never the token.
func TestLongLoginOverHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["expires_in"] == nil {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "LONG-SECRET", "expires_in": body["expires_in"],
			"account": map[string]string{"uuid": "u1", "email_address": "a@x"}})
	}))
	defer srv.Close()
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	defer func() { rota.ClaudeEndpoints.Token = old }()

	h := newHarness(t, Options{})
	// The account the token will belong to has to be here already, named by
	// the uuid the approval carries.
	seedUUID(t, h.dir, 1, "u1")

	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "claude", "long": true})
	var login struct {
		ID   string `json:"id"`
		URL  string `json:"url"`
		Long bool   `json:"long"`
	}
	json.Unmarshal(raw, &login)
	if resp.StatusCode != 200 || !login.Long || !strings.Contains(login.URL, "scope=user%3Ainference&") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}

	resp, raw = h.do("POST", "/v1/login/"+login.ID, map[string]string{"code": "CODE"})
	var fin struct {
		ID        int    `json:"id"`
		Status    string `json:"status"`
		LongUntil string `json:"long_until"`
	}
	json.Unmarshal(raw, &fin)
	if resp.StatusCode != 200 || fin.ID != 1 || fin.Status != "long" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if _, err := time.Parse(time.RFC3339, fin.LongUntil); err != nil {
		t.Fatalf("long_until: %q", fin.LongUntil)
	}
	if strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("the token must not come back: %s", raw)
	}

	resp, raw = h.do("GET", "/v1/accounts", nil)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"long_until"`) || strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}

	// And forgetting it is a setting like any other.
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]string{"long": "drop"}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "forget") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]string{"long": "forget"}); resp.StatusCode != 200 ||
		strings.Contains(string(raw), "long_until") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	blob, _ := os.ReadFile(filepath.Join(h.dir, "accounts.json"))
	if strings.Contains(string(blob), "LONG-SECRET") {
		t.Fatalf("still stored: %s", blob)
	}
}

// seedUUID gives one of the harness's accounts the uuid an approval will
// name, so the long token has an account to belong to.
func seedUUID(t *testing.T, dir string, id int, uuid string) {
	t.Helper()
	path := filepath.Join(dir, "accounts.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
		NextID   int              `json:"nextId"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if int(a["id"].(float64)) == id {
			a["uuid"] = uuid
		}
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
