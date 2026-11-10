package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		id, _ := doc["id"].(string)
		url, _ := doc["url"].(string)
		created, _ := doc["createdAt"].(float64)
		if resp.StatusCode != 200 || id == "" || url == "" || created <= 0 || doc["kind"] != "code" || doc["provider"] != "claude" {
			t.Fatalf("%v: %d %s", body, resp.StatusCode, raw)
		}
		if _, ok := doc["state"]; ok || strings.Contains(string(raw), "verifier") {
			t.Fatalf("%v: the state went out: %s", body, raw)
		}
		if _, ok := doc["delegated"]; ok {
			t.Fatalf("%v: delegated is said only when it is so: %s", body, raw)
		}
	}
	// A login that can also be delegated says so: the playground offers the
	// keyless route on it.
	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "grok"})
	if doc := beginReply(t, raw); resp.StatusCode != 200 || doc["delegated"] != true || doc["kind"] != "apikey" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// boundIn is which parked logins the store says were begun for an account.
func boundIn(t *testing.T, dir string) map[string]int {
	t.Helper()
	m := map[string]int{}
	raw, err := os.ReadFile(filepath.Join(dir, "homes", "pending-for.json"))
	if err == nil {
		err = json.Unmarshal(raw, &m)
	}
	if err != nil {
		t.Fatalf("the bindings: %v %s", err, raw)
	}
	return m
}

// addAccounts puts these accounts in the harness's store beside the ones it
// was made with.
func addAccounts(t *testing.T, dir string, accounts ...map[string]any) {
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
	for _, a := range accounts {
		doc.Accounts = append(doc.Accounts, a)
		if id := int(a["id"].(float64)); id >= doc.NextID {
			doc.NextID = id + 1
		}
	}
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// {"account":N} begins the login for that account, as `rota login N` does:
// one body, one operation.
func TestBeginningALoginForAnAccountOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	addAccounts(t, h.dir,
		map[string]any{"id": float64(5), "provider": "claude", "uuid": "key-5", "credential": "apikey",
			"token": map[string]any{"accessToken": "sk-ant-api-xxxxWXYZ"}},
		map[string]any{"id": float64(6), "provider": "grok", "uuid": "grok-6", "delegated": true,
			"token": map[string]any{}})
	resp, raw := h.do("POST", "/v1/login", map[string]any{"account": 1})
	doc := beginReply(t, raw)
	url, _ := doc["url"].(string)
	if resp.StatusCode != 200 || doc["account"] != float64(1) || doc["kind"] != "code" || url == "" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if _, ok := doc["state"]; ok {
		t.Fatalf("the state went out: %s", raw)
	}
	id, _ := doc["id"].(string)
	var kept map[string]rota.Login
	raw, err := os.ReadFile(filepath.Join(h.dir, "homes", "pending.json"))
	if err := errors.Join(err, json.Unmarshal(raw, &kept)); err != nil || kept[id].State["verifier"] == "" {
		t.Fatalf("not parked: %v %s", err, raw)
	}
	if b := boundIn(t, h.dir); b[id] != 1 {
		t.Fatalf("not bound to account 1: %v", b)
	}

	for _, c := range []struct {
		body map[string]any
		code int
		says string
	}{
		{map[string]any{"account": 99}, 404, "no such account"},
		{map[string]any{"account": 1, "long": true}, 400, "takes no long or provider"},
		{map[string]any{"account": 1, "provider": "codex"}, 400, "takes no long or provider"},
		{map[string]any{"account": -1}, 400, "account's id"},
		// Accounts with no sign-in for rota to start.
		{map[string]any{"account": 5}, 400, "rota login --key"},
		{map[string]any{"account": 6}, 400, "its own CLI"},
	} {
		resp, raw := h.do("POST", "/v1/login", c.body)
		if resp.StatusCode != c.code || !strings.Contains(string(raw), c.says) {
			t.Errorf("%v: %d %s", c.body, resp.StatusCode, raw)
		}
		if strings.Contains(string(raw), "WXYZ") {
			t.Errorf("%v: part of a key in the answer: %s", c.body, raw)
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
	if p, err := s.PendingLogin(l.ID); err != nil || p == nil {
		t.Fatalf("the parked login must stay: %+v %v", p, err)
	}
	if b := boundIn(t, h.dir); b[l.ID] != 1 {
		t.Fatalf("and stay bound: %v", b)
	}
}
