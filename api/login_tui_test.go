package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// fakeClaudeToken points claude's token and profile endpoints at answer for
// the rest of the test.
func fakeClaudeToken(t *testing.T, answer http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(answer)
	t.Cleanup(srv.Close)
	for _, e := range []*string{&rota.ClaudeEndpoints.Token, &rota.ClaudeEndpoints.Profile} {
		old := *e
		t.Cleanup(func() { *e = old })
		*e = srv.URL
	}
}

// signedIn is a token reply naming this account.
func signedIn(w http.ResponseWriter, uuid, email string) {
	json.NewEncoder(w).Encode(map[string]any{"access_token": "NEW-SECRET", "refresh_token": "NEW-REFRESH", "expires_in": 3600,
		"account": map[string]string{"uuid": uuid, "email_address": email}})
}

// parked says whether the store still keeps this login.
func parked(t *testing.T, dir, id string) bool {
	t.Helper()
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := s.PendingLogin(id)
	if err != nil {
		t.Fatal(err)
	}
	return p != nil
}

// A refusal approving again cannot put right says so, for a client that would
// otherwise keep offering the approval.
func TestAnUnfinishableRefusalSaysSo(t *testing.T) {
	fakeClaudeToken(t, func(w http.ResponseWriter, r *http.Request) { signedIn(w, "u1", "a@x") })
	h := newHarness(t, Options{})
	seedUUID(t, h.dir, 1, "u1")
	resp, raw := h.do("POST", "/v1/login", map[string]any{"account": 1})
	id, _ := beginReply(t, raw)["id"].(string)
	if resp.StatusCode != 200 || id == "" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	s, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Remove(1)
	if err == nil {
		err = s.Save()
	}
	s.Close()
	if err != nil {
		t.Fatal(err)
	}

	resp, raw = h.do("POST", "/v1/login/"+id, map[string]string{"code": "CODE"})
	doc := beginReply(t, raw)
	if resp.StatusCode != 400 || doc["reason"] != "unfinishable" || !strings.Contains(doc["error"].(string), "no longer here") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if strings.Contains(string(raw), "NEW-SECRET") {
		t.Fatalf("a token in the answer: %s", raw)
	}
	if !parked(t, h.dir, id) {
		t.Fatal("the login must stay parked")
	}

	// A refusal approving again can fix carries no reason.
	seedUUID(t, h.dir, 3, "u3")
	resp, raw = h.do("POST", "/v1/login", map[string]any{"account": 3})
	id, _ = beginReply(t, raw)["id"].(string)
	resp, raw = h.do("POST", "/v1/login/"+id, map[string]string{"code": "CODE"})
	if doc := beginReply(t, raw); resp.StatusCode != 400 || doc["reason"] != nil {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// An account whose own CLI signs it in is sent there, by a reason a client
// reads rather than a sentence, on both of the ways the store finds it out.
func TestADelegatedAccountIsSentToItsCLI(t *testing.T) {
	h := newHarness(t, Options{})
	addAccounts(t, h.dir,
		// Known as delegated by the store.
		map[string]any{"id": float64(6), "provider": "grok", "uuid": "grok-6", "delegated": true,
			"token": map[string]any{}},
		// Known only once its provider's login is begun.
		map[string]any{"id": float64(7), "provider": "kimi", "uuid": "kimi-7", "token": map[string]any{"accessToken": "k"}},
		map[string]any{"id": float64(5), "provider": "claude", "uuid": "key-5", "credential": "apikey",
			"token": map[string]any{"accessToken": "sk-ant-api-xxxxWXYZ"}})
	for _, id := range []int{6, 7} {
		resp, raw := h.do("POST", "/v1/login", map[string]any{"account": id})
		doc := beginReply(t, raw)
		if resp.StatusCode != 400 || doc["reason"] != "delegated" || !strings.Contains(doc["error"].(string), "its own CLI") {
			t.Errorf("account %d: %d %s", id, resp.StatusCode, raw)
		}
	}
	// An API key has no sign-in either, but no CLI to send it to.
	resp, raw := h.do("POST", "/v1/login", map[string]any{"account": 5})
	if doc := beginReply(t, raw); resp.StatusCode != 400 || doc["reason"] != nil {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// The provider turning a code down is the caller's mistake, said in the
// provider's words, and the same login takes the right code.
func TestAWrongCodeIsABadRequest(t *testing.T) {
	fakeClaudeToken(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] == "authorization_code" && body["code"] != "RIGHT" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid authorization code"}`))
			return
		}
		signedIn(w, "u-new", "new@x")
	})
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/login", map[string]any{"provider": "claude"})
	id, _ := beginReply(t, raw)["id"].(string)
	if resp.StatusCode != 200 || id == "" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	resp, raw = h.do("POST", "/v1/login/"+id, map[string]string{"code": "WRONG"})
	doc := beginReply(t, raw)
	msg, _ := doc["error"].(string)
	if resp.StatusCode != 400 || doc["reason"] != nil || !strings.Contains(msg, "Invalid authorization code") ||
		!strings.HasSuffix(msg, "try the code again, or approve again at the same link") || strings.Contains(msg, "start a new login") {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
	if !parked(t, h.dir, id) {
		t.Fatal("the login must stay parked")
	}
	resp, raw = h.do("POST", "/v1/login/"+id, map[string]string{"code": "RIGHT"})
	if doc := beginReply(t, raw); resp.StatusCode != 200 || doc["status"] != "added" || doc["email"] != "new@x" {
		t.Fatalf("%d %s", resp.StatusCode, raw)
	}
}

// staleMeter is a metered provider that counts the usage reads asked of it,
// by the account's token.
type staleMeter struct{ apiKeyProvider }

var staleReads = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

func (staleMeter) Name() string { return "t-api-stale" }
func (staleMeter) Quota(_ context.Context, token string) (*rota.Quota, error) {
	staleReads.Lock()
	defer staleReads.Unlock()
	staleReads.n[token]++
	return &rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 7, Primary: true}}}, nil
}

func reads(token string) int {
	staleReads.Lock()
	defer staleReads.Unlock()
	return staleReads.n[token]
}

// ?refresh=stale reads what a listing would, and leaves the rest alone.
func TestStaleRefreshReadsOnlyWhatIsStale(t *testing.T) {
	rota.Register(staleMeter{})
	h := newHarness(t, Options{})
	st, err := store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	far := rota.NowMS() + 3_600_000
	st.Accounts = append(st.Accounts,
		&rota.Account{ID: 8, Provider: "t-api-stale", Order: 8, Token: rota.Token{Access: "fresh", ExpiresAt: far},
			Quota:   &rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 1, Primary: true}}},
			QuotaAt: time.Now().Add(-time.Minute).UnixMilli()},
		&rota.Account{ID: 9, Provider: "t-api-stale", Order: 9, Token: rota.Token{Access: "never", ExpiresAt: far}})
	err = st.Save()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	list := func(q string) {
		t.Helper()
		if resp, raw := h.do("GET", "/v1/accounts?refresh="+q, nil); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", q, resp.StatusCode, raw)
		}
	}
	list("stale")
	if reads("fresh") != 0 || reads("never") != 1 {
		t.Fatalf("fresh %d, never %d", reads("fresh"), reads("never"))
	}
	list("stale")
	if reads("fresh") != 0 || reads("never") != 1 {
		t.Fatalf("read again while fresh: fresh %d, never %d", reads("fresh"), reads("never"))
	}
	// Once its reading is past the cache's age, it is read again, with no
	// cooldown between.
	st, err = store.Open(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	st.Find(9).QuotaAt = time.Now().Add(-store.QuotaTTL - time.Minute).UnixMilli()
	err = st.Save()
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	list("stale")
	if reads("fresh") != 0 || reads("never") != 2 {
		t.Fatalf("once stale: fresh %d, never %d", reads("fresh"), reads("never"))
	}

	// ?refresh=1 still forces, once a minute per account, stale reads or not.
	list("1")
	if reads("fresh") != 1 || reads("never") != 3 {
		t.Fatalf("forced: fresh %d, never %d", reads("fresh"), reads("never"))
	}
	list("1")
	if reads("fresh") != 1 || reads("never") != 3 {
		t.Fatalf("forced within the minute: fresh %d, never %d", reads("fresh"), reads("never"))
	}
}
