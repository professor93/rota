package store

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// claudeFake stands in for the provider's token endpoint and counts what was
// asked of it, so a test can say not only what a launch produced but what it
// did not bother the provider with.
type claudeFake struct {
	refreshes atomic.Int64
	exchanges atomic.Int64
	// reply is what the authorization-code exchange answers with; nil is a
	// token for the account below.
	identity map[string]string
	refuse   bool
}

func newClaudeFake(t *testing.T, f *claudeFake) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["grant_type"] == "refresh_token" {
			f.refreshes.Add(1)
			if f.refuse {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh", "expires_in": 3600})
			return
		}
		f.exchanges.Add(1)
		out := map[string]any{"access_token": "LONG-SECRET", "expires_in": body["expires_in"]}
		if f.identity != nil {
			out["account"] = f.identity
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL + "/token"
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })
}

// claudeAccount is an ordinary claude account with an expired access token,
// so anything that means to refresh has to.
func claudeAccount(s *Store, uuid, email string) *rota.Account {
	a := s.add("claude")
	a.UUID, a.Email = uuid, email
	a.Token = rota.Token{Access: "stale", Refresh: "r", ExpiresAt: 1}
	return a
}

func TestLongLoginAttachesToTheAccountThatApprovedIt(t *testing.T) {
	f := &claudeFake{identity: map[string]string{"uuid": "u1", "email_address": "one@x"}}
	newClaudeFake(t, f)
	s := openTemp(t)
	other := claudeAccount(s, "u0", "zero@x")
	a := claudeAccount(s, "u1", "one@x")
	before := len(s.Accounts)

	l, err := s.BeginLongLogin(context.Background(), "claude")
	if err != nil || !l.Long || !strings.Contains(l.URL, "scope=user%3Ainference&") {
		t.Fatalf("begin: %+v %v", l, err)
	}
	if got, err := s.PendingLogin(l.ID); err != nil || got == nil || !got.Long {
		t.Fatalf("a parked long login must say so: %+v %v", got, err)
	}
	got, added, err := s.FinishLogin(context.Background(), l.ID, "CODE")
	if err != nil || added || got.ID != a.ID {
		t.Fatalf("finish: %+v added=%v %v", got, added, err)
	}
	if len(s.Accounts) != before {
		t.Fatal("a long login must never create an account")
	}
	if a.Long == nil || a.Long.Access != "LONG-SECRET" || !a.LongValid() {
		t.Fatalf("long token not stored: %+v", a.Long)
	}
	if a.Token.Access != "stale" || a.Token.Refresh != "r" {
		t.Fatal("the ordinary credential must be left exactly as it was")
	}
	if other.Long != nil {
		t.Fatal("only the account that approved gets the token")
	}
	// A year, from the expires_in the exchange asked for and the endpoint
	// echoed back.
	if until := a.LongUntil(); until.Before(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("until: %v", until)
	}
	// And it survives a reload, since the whole point is a credential that
	// outlives this process.
	s.Close()
	s2, _ := Open(storeDir(s))
	defer s2.Close()
	if b := s2.Find(a.ID); b.Long == nil || b.Long.Access != "LONG-SECRET" {
		t.Fatalf("not persisted: %+v", b.Long)
	}
}

func TestLongLoginRefusesAnIdentityItDoesNotKnow(t *testing.T) {
	for _, c := range []struct {
		what     string
		identity map[string]string
		says     string
	}{
		{"a stranger", map[string]string{"uuid": "u9", "email_address": "nine@x"}, "nine@x"},
		{"nobody at all", nil, "named no account"},
	} {
		f := &claudeFake{identity: c.identity}
		newClaudeFake(t, f)
		s := openTemp(t)
		a := claudeAccount(s, "u1", "one@x")
		l, _ := s.BeginLongLogin(context.Background(), "claude")
		_, _, err := s.FinishLogin(context.Background(), l.ID, "CODE")
		if err == nil || !strings.Contains(err.Error(), c.says) {
			t.Fatalf("%s: %v", c.what, err)
		}
		if len(s.Accounts) != 1 || a.Long != nil {
			t.Fatalf("%s: nothing may be created or stored", c.what)
		}
		// The parked login stays: approving again as the right account and
		// pasting that code finishes this same login.
		if got, _ := s.PendingLogin(l.ID); got == nil {
			t.Fatalf("%s: the login must still be there to retry", c.what)
		}
		s.Close()
	}
}

func TestALaunchWithALongTokenAsksTheProviderForNothing(t *testing.T) {
	f := &claudeFake{refuse: true}
	newClaudeFake(t, f)
	onPath(t, "claude")
	s := openTemp(t)
	a := claudeAccount(s, "u1", "one@x")
	a.Long = &rota.LongToken{Access: "LONG-SECRET", ExpiresAt: time.Now().Add(300 * 24 * time.Hour).UnixMilli()}

	_, env, release, err := s.Prepare(context.Background(), a)
	if release != nil {
		release()
	}
	// The refresh endpoint refuses everything, and the run happens anyway:
	// it never needed that lineage.
	if err != nil {
		t.Fatalf("a run on a long token must not depend on a refresh: %v", err)
	}
	if n := f.refreshes.Load(); n != 0 {
		t.Fatalf("the refresh endpoint was called %d times", n)
	}
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=LONG-SECRET") {
		t.Fatal("the long token is what the CLI must be launched with")
	}
	if a.Dead {
		t.Fatal("a refresh that was never made cannot have killed the account")
	}

	// Without one, the same launch refreshes — and dies on that refusal, as
	// it always did.
	b := claudeAccount(s, "u2", "two@x")
	if _, _, _, err := s.Prepare(context.Background(), b); err == nil || !b.Dead {
		t.Fatalf("err=%v dead=%v", err, b.Dead)
	}
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("refreshes=%d", n)
	}
}

func TestADeadAccountRunsWhenItHasALongTokenAndSaysSoOnce(t *testing.T) {
	newClaudeFake(t, &claudeFake{})
	onPath(t, "claude")
	s := openTemp(t)
	var said []string
	s.Warn = func(msg string) { said = append(said, msg) }
	a := claudeAccount(s, "u1", "one@x")
	a.Dead, a.DeadReason = true, "invalid_grant: refresh token reused"

	if _, _, _, err := s.Prepare(context.Background(), a); err == nil {
		t.Fatal("a dead account with nothing else must still be refused")
	}
	a.Long = &rota.LongToken{Access: "LONG-SECRET", ExpiresAt: time.Now().Add(200 * 24 * time.Hour).UnixMilli()}
	_, env, release, err := s.Prepare(context.Background(), a)
	if release != nil {
		release()
	}
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=LONG-SECRET") {
		t.Fatalf("err=%v", err)
	}
	if len(said) != 1 || !strings.Contains(said[0], "invalid_grant: refresh token reused") ||
		!strings.Contains(said[0], "usage is unknown") || !strings.Contains(said[0], a.LongUntil().Format(time.DateOnly)) {
		t.Fatalf("what was said: %q", said)
	}
	if strings.Contains(strings.Join(said, " "), "LONG-SECRET") {
		t.Fatal("the token itself is never said out loud")
	}
	if a.Status() != rota.StatusReauth {
		t.Fatal("it is still a dead login, and every listing must keep saying so")
	}
}

// Nothing rota writes for a person or a program carries the token: only the
// store file does, and that is the file the refresh tokens are already in.
func TestTheLongTokenIsNowhereButTheStore(t *testing.T) {
	s := openTemp(t)
	a := claudeAccount(s, "u1", "one@x")
	a.Long = &rota.LongToken{Access: "LONG-SECRET", ExpiresAt: time.Now().Add(100 * 24 * time.Hour).UnixMilli()}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := s.Backend().Load()
	if err != nil || !strings.Contains(string(raw), "LONG-SECRET") {
		t.Fatalf("the store is where it lives: %v", err)
	}
	fi, err := os.Stat(filepath.Join(storeDir(s), "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("and the store is private: mode %o", fi.Mode().Perm())
	}
}
