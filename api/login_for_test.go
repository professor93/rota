package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// beginReply is POST /v1/login's answer, as a client reads it.
func beginReply(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return doc
}

// What a login keeps for its second half, the PKCE verifier above all, stays
// with the parked login: the reply says what to open and how to finish it.
func TestBeginningALoginOverHTTPKeepsItsStateToItself(t *testing.T) {
	h := newHarness(t, Options{})
	for _, body := range []map[string]any{{"provider": "claude"}, {"provider": "claude", "long": true}, {}} {
		resp, raw := h.do("POST", "/v1/login", body)
		doc := beginReply(t, raw)
		if resp.StatusCode != 200 || doc["id"] == "" || doc["url"] == "" || doc["kind"] != "code" || doc["provider"] != "claude" {
			t.Fatalf("%v: %d %s", body, resp.StatusCode, raw)
		}
		if _, ok := doc["state"]; ok || strings.Contains(string(raw), "verifier") {
			t.Fatalf("%v: the state went out: %s", body, raw)
		}
	}
}

// {"account":N} begins the login for that account, as `rota login N` does:
// one body, one operation.
func TestBeginningALoginForAnAccountOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	resp, raw := h.do("POST", "/v1/login", map[string]any{"account": 1})
	doc := beginReply(t, raw)
	if resp.StatusCode != 200 || doc["account"] != float64(1) || doc["kind"] != "code" || doc["url"] == "" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if _, ok := doc["state"]; ok {
		t.Fatalf("the state went out: %s", raw)
	}
	s, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.PendingLogin(doc["id"].(string))
	s.Close()
	if err != nil || l == nil || l.Account != 1 || l.State["verifier"] == "" {
		t.Fatalf("not parked for account 1: %+v %v", l, err)
	}

	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"account": 99}, 404},
		{map[string]any{"account": 1, "long": true}, 400},
		{map[string]any{"account": 1, "provider": "codex"}, 400},
		{map[string]any{"account": -1}, 400},
	} {
		if resp, raw := h.do("POST", "/v1/login", c.body); resp.StatusCode != c.code {
			t.Errorf("%v: %d %s", c.body, resp.StatusCode, raw)
		}
	}
}

// A login begun for one account is held to it wherever it is finished, and
// the finish that is refused over HTTP leaves everything as it found it.
func TestFinishingALoginBegunForAnAccountOverHTTPIsHeldToIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "WRONG-SECRET", "refresh_token": "WRONG-REFRESH", "expires_in": 3600,
			"account": map[string]string{"uuid": "u-stranger", "email_address": "stranger@x"}})
	}))
	defer srv.Close()
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
		*e = srv.URL
	}

	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	s, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.BeginLoginFor(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// The first look at a seeded store numbers its queue and saves that, which
	// is nothing this test is about.
	if resp, raw := h.do("GET", "/v1/accounts", nil); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	path := filepath.Join(h.dir, "accounts.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	resp, raw := h.do("POST", "/v1/login/"+l.ID, map[string]string{"code": "CODE"})
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "stranger@x") || !strings.Contains(string(raw), "account 1") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "WRONG-SECRET") || strings.Contains(string(raw), "WRONG-REFRESH") {
		t.Fatalf("a token in the answer: %s", raw)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("written: %v\n%s\n%s", err, before, after)
	}

	s, err = store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, err := s.PendingLogin(l.ID); err != nil || p == nil || p.Account != 1 {
		t.Fatalf("the parked login must stay: %+v %v", p, err)
	}
}
