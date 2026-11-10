package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
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
	if resp, raw := h.socket().do("PATCH", "/v1/accounts/"+keyed, map[string]any{"remote_control": true}); resp.StatusCode != 400 ||
		!strings.Contains(string(raw), "API key") {
		t.Fatalf("remote control refuses a key: %d %s", resp.StatusCode, raw)
	}

	// A key sent as a directory is refused, not stored and listed.
	for _, field := range []string{"cwd", "config_dir", "sessions"} {
		resp, raw := h.do("PATCH", "/v1/accounts/1", map[string]any{field: "/srv/" + testKey})
		clean(raw)
		if resp.StatusCode != 400 || !strings.Contains(string(raw), "looks like a key") {
			t.Fatalf("%s: %d %s", field, resp.StatusCode, raw)
		}
	}
	if _, raw := h.do("GET", "/v1/accounts", nil); strings.Contains(string(raw), "TESTONLY") {
		t.Fatal("a key was kept as a directory")
	}

	resp, raw = h.do("POST", "/v1/login", map[string]any{"account": 1, "token": testSetup})
	clean(raw)
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"status":"long"`) ||
		!strings.Contains(string(raw), "cannot tell which claude.ai account") {
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

// A Console API key sent as a setup token is refused here, for what it is,
// and never sent to the API to be refused there as a bad key.
func TestAKeySentAsATokenNeverLeaves(t *testing.T) {
	var asked atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		w.WriteHeader(401)
	}))
	defer api.Close()
	old := rota.ClaudeModelsEndpoint
	rota.ClaudeModelsEndpoint = api.URL
	defer func() { rota.ClaudeModelsEndpoint = old }()

	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	resp, raw := h.do("POST", "/v1/login", map[string]any{"account": 1, "token": testKey})
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || resp.StatusCode != 400 ||
		!strings.Contains(got.Error, "Console API key") || !strings.Contains(got.Error, "rota login --key") ||
		strings.Contains(got.Error, "refused this key") || strings.Contains(string(raw), "TESTONLY") {
		t.Fatalf("%d %s", resp.StatusCode, strings.ReplaceAll(string(raw), testKey, "<key>"))
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("a key sent as a setup token must go nowhere: %d requests reached the API", n)
	}
}

// A run over HTTP that the API refused for want of credit answers as any
// failed run does, and leaves its key paused: the listing says limited and
// until when, and the rotation names another account.
func TestAKeyOutOfCreditOverHTTP(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer api.Close()
	old := rota.ClaudeModelsEndpoint
	rota.ClaudeModelsEndpoint = api.URL
	defer func() { rota.ClaudeModelsEndpoint = old }()

	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	h := newHarness(t, Options{})
	_, raw := h.do("POST", "/v1/login", map[string]any{"provider": "claude", "key": testKey})
	var got struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.ID == 0 {
		t.Fatalf("login: %s", raw)
	}
	id := strconv.Itoa(got.ID)
	if resp, raw := h.do("PATCH", "/v1/accounts/"+id, map[string]any{"order": 1}); resp.StatusCode != 200 {
		t.Fatalf("into the rotation, first: %d %s", resp.StatusCode, raw)
	}

	bin := t.TempDir()
	refusal := fakecli.Lines(`{"type":"result","subtype":"success","is_error":true,"api_error_status":400,` +
		`"num_turns":1,"result":"Credit balance is too low","session_id":"s-credit","total_cost_usd":0}`)
	refusal.Exit = 1
	fakecli.Install(t, bin, "claude", refusal)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	resp, raw := h.do("POST", "/v1/accounts/"+id+"/run", map[string]any{"prompt": "hi"})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "Credit balance is too low") {
		t.Fatalf("a failed run, as ever: %d %s", resp.StatusCode, raw)
	}
	_, raw = h.do("GET", "/v1/accounts", nil)
	var doc struct {
		Accounts []struct {
			ID            int    `json:"id"`
			Status        string `json:"status"`
			NoCreditUntil string `json:"no_credit_until"`
		} `json:"accounts"`
		Default int `json:"default"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.ID == got.ID && (a.Status != "limited" || a.NoCreditUntil == "") {
			t.Fatalf("the key is paused: %+v", a)
		}
	}
	if doc.Default == got.ID {
		t.Fatalf("and the rotation passes it by: %s", raw)
	}
}
