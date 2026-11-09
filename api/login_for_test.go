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
