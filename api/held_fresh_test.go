//go:build linux || darwin

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// heldLogin rewrites the harness's store so that accounts 1 and 3 are claude
// logins kept in their homes, in sync on R<id>, and gives them those homes.
func heldLogin(t *testing.T, p *terms, expires time.Time) {
	t.Helper()
	var accounts []*rota.Account
	for _, id := range []int{1, 3} {
		rt, at := "R"+strconv.Itoa(id), "A"+strconv.Itoa(id)
		a := &rota.Account{ID: id, Provider: "claude", UUID: "u" + strconv.Itoa(id), Email: "u" + strconv.Itoa(id) + "@x",
			Token: rota.Token{Access: at, Refresh: rt, ExpiresAt: expires.UnixMilli(), Scopes: []string{"user:inference"}},
			Extra: map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "route": "stored"}}
		a.StagedWritten()
		accounts = append(accounts, a)
		writeHomeLogin(t, p, id, at, rt, expires)
	}
	raw, _ := json.Marshal(map[string]any{"accounts": accounts, "nextId": 4})
	if err := os.WriteFile(filepath.Join(p.dir, "accounts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeHomeLogin(t *testing.T, p *terms, id int, access, refresh string, expires time.Time) {
	t.Helper()
	home := filepath.Join(p.dir, "homes", "claude-"+strconv.Itoa(id))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	login, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh,
		"expiresAt": expires.UnixMilli(), "refreshTokenExpiresAt": 1999999999000, "scopes": []string{"user:inference"}}})
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), login, 0o600); err != nil {
		t.Fatal(err)
	}
}

// storeHolds waits for the store on disk to hold the refresh token.
func storeHolds(t *testing.T, p *terms, rt string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		raw, _ := os.ReadFile(filepath.Join(p.dir, "accounts.json"))
		if strings.Contains(string(raw), `"`+rt+`"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the store never took %s: %s", rt, raw)
		}
	}
}

func lookEvery(t *testing.T, d time.Duration) {
	t.Helper()
	old := heldLookEvery
	heldLookEvery = func() time.Duration { return d }
	t.Cleanup(func() { heldLookEvery = old })
}

// A held claude account's rotation in its home is followed with the sweeps
// turned off: the looks at held sessions do not depend on RefreshEvery.
func TestAHeldAccountsRotationIsFollowedWithTheSweepsOff(t *testing.T) {
	lookEvery(t, 20*time.Millisecond)
	p := withHeld(t, nil, func(o *Options) { o.RefreshEvery = -1 })
	later := time.Now().Add(time.Hour)
	heldLogin(t, p, later)
	p.start(map[string]any{"account": 1, "cols": 80, "rows": 24})
	writeHomeLogin(t, p, 1, "A1b", "R1b", later.Add(8*time.Hour))
	storeHolds(t, p, "R1b")
}

// One look is made as the server starts, for the sessions it has taken up: a
// Claude Code that rotated while no daemon ran is followed long before the
// first interval is over.
func TestAHeldAccountIsLookedAtAsTheServerStarts(t *testing.T) {
	lookEvery(t, time.Hour)
	p := withHeld(t, nil, func(o *Options) { o.RefreshEvery = -1 })
	later := time.Now().Add(time.Hour)
	heldLogin(t, p, later)
	p.start(map[string]any{"account": 1, "cols": 80, "rows": 24})
	p.api.Stop()
	writeHomeLogin(t, p, 1, "A1b", "R1b", later.Add(8*time.Hour))
	storeHolds(t, p.restart(), "R1b")
}

// The looks are at the accounts of held sessions still running, and they only
// look: an expired token in a quiet home is not refreshed by them, nor is a home
// nobody holds a session in followed, nor one whose session has ended.
func TestTheLooksAreOnlyAtRunningSessionsAndPresentNothing(t *testing.T) {
	var presented atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		presented.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
	lookEvery(t, time.Hour)
	p := withHeld(t, nil, func(o *Options) { o.RefreshEvery = -1 })
	expired := time.Now().Add(-time.Minute)
	heldLogin(t, p, expired)
	doc := p.start(map[string]any{"account": 1, "cols": 80, "rows": 24})
	p.api.lookAtHeld()
	if n := presented.Load(); n != 0 {
		t.Fatalf("a look at a held session's home refreshed an expired token: %d", n)
	}
	// Nobody holds a session on account 3.
	later := time.Now().Add(9 * time.Hour)
	writeHomeLogin(t, p, 3, "A3b", "R3b", later)
	p.api.lookAtHeld()
	raw, _ := os.ReadFile(filepath.Join(p.dir, "accounts.json"))
	if strings.Contains(string(raw), "R3b") {
		t.Fatalf("a home no held session runs in was followed: %s", raw)
	}
	// Nor a session that has ended.
	p.api.termsMu.Lock()
	ts := p.api.terms[doc["id"].(string)]
	p.api.termsMu.Unlock()
	ts.back.(*heldTerm).setState(heldEnded, 0)
	writeHomeLogin(t, p, 1, "A1b", "R1b", later)
	p.api.lookAtHeld()
	raw, _ = os.ReadFile(filepath.Join(p.dir, "accounts.json"))
	if strings.Contains(string(raw), "R1b") {
		t.Fatalf("the home of a session that has ended was followed: %s", raw)
	}
}

// The looks stop with the sweeps, as the server's stop begins.
func TestTheLooksAtHeldSessionsStopWithTheSweeps(t *testing.T) {
	lookEvery(t, time.Hour)
	p := withHeld(t, nil, func(o *Options) { o.RefreshEvery = -1 })
	p.api.StopRefreshing()
	wait(t, p.api.heldDone, "the looks to end once the sweeps were told to")
	if p.api.ctx.Err() != nil {
		t.Fatal("stopping the sweeps stopped the server")
	}
}
