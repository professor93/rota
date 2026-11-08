package rotation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// The rotation reads usage before it chooses, and a token that has expired is
// refreshed for that reading. Its caller giving up on the choice — a request
// that hung up — does not give up on that refresh: the provider has rotated
// the login by the time it answers, and the answer is the only copy of the
// new token. rota waits for it, saves it and writes it into the home, and the
// token is presented once.
func TestChooseKeepsTheRefreshItsCallerGaveUpOn(t *testing.T) {
	var (
		mu        sync.Mutex
		presented []string
	)
	arrived, abandoned, release := make(chan string, 4), make(chan string, 4), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			var req struct {
				Refresh string `json:"refresh_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			presented = append(presented, req.Refresh)
			mu.Unlock()
			arrived <- req.Refresh
			select {
			case <-release:
			case <-r.Context().Done():
				abandoned <- req.Refresh
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "A1", "refresh_token": "R1", "expires_in": 28800})
		case "/usage":
			_ = json.NewEncoder(w).Encode(map[string]any{"five_hour": map[string]any{"utilization": 10}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo) // runs before Close, which would wait on a held answer
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}

	// One claude account, expired, its login kept in its home as rota wrote it
	// there, and nothing alive in the home.
	dir := filepath.Join(t.TempDir(), "rota")
	expired := time.Now().Add(-time.Minute).UnixMilli()
	a := &rota.Account{ID: 1, Provider: "claude", UUID: "u1", Email: "u1@x", Order: 1,
		Token: rota.Token{Access: "A0", Refresh: "R0", ExpiresAt: expired, Scopes: []string{"user:inference"}},
		Extra: map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "route": "stored"}}
	a.StagedWritten()
	home := filepath.Join(dir, "homes", "claude-1")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "A0", "refreshToken": "R0",
		"expiresAt": expired, "refreshTokenExpiresAt": 1999999999000, "scopes": []string{"user:inference"}}})
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"accounts": []*rota.Account{a}, "nextId": 2})
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	chosen := make(chan *rota.Account, 1)
	go func() {
		got, _ := Choose(ctx, st, 0)
		chosen <- got
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no refresh reached the provider")
	}
	giveUp()
	select {
	case rt := <-abandoned:
		t.Fatalf("rota gave up on the provider's answer to %s, which has rotated it: the new token is lost", rt)
	case <-time.After(300 * time.Millisecond):
	}
	letGo()
	select {
	case <-chosen:
	case <-time.After(10 * time.Second):
		t.Fatal("Choose did not return once the refresh was answered")
	}
	st.Close()

	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	saved := st.Find(1)
	st.Close()
	if saved == nil || saved.Token.Refresh != "R1" {
		t.Errorf("the store holds %+v, not the refreshed login", saved)
	}
	if runtime.GOOS != "windows" { // no login is kept in a home there
		raw, _ := os.ReadFile(filepath.Join(home, ".credentials.json"))
		var doc struct {
			Login struct {
				Refresh string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		_ = json.Unmarshal(raw, &doc)
		if doc.Login.Refresh != "R1" {
			t.Errorf("the home holds %q, not the refreshed login", doc.Login.Refresh)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(presented) != 1 || presented[0] != "R0" {
		t.Errorf("presented %v, want R0 once", presented)
	}
}
