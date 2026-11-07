package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// refreshOnlyClaude adds a claude account rota holds a refresh token for and
// nothing else: no access token, and so no expiry.
func refreshOnlyClaude(s *Store, uuid string) *rota.Account {
	a := s.add("claude")
	a.UUID, a.Email = uuid, uuid+"@x"
	a.Token = rota.Token{Refresh: "R-" + uuid}
	a.StagedSuperseded()
	return a
}

// noEmptyAccessToken fails when the home's credential store holds a login
// whose access token is empty.
func noEmptyAccessToken(t *testing.T, home string) {
	t.Helper()
	_, doc := readLogin(t, home)
	if doc == nil {
		return
	}
	if l, _ := doc["claudeAiOauth"].(map[string]any); l["accessToken"] == "" || l["accessToken"] == nil {
		t.Fatalf("a login with an empty access token was written into the home: %v", doc)
	}
}

// tokenVars is what of an environment hands Claude Code a token, for a
// failure to show without the rest of the machine's variables.
func tokenVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(e string) bool { return !strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") })
}

// A claude account held as a refresh token alone is refreshed before its
// first run, the refresh saved before anything starts, and the run goes on
// the access token the refresh gave it. That first run is on a token, since
// the account had no expiry to take its stored route by when the route was
// chosen; the next one takes it, and the home then holds the refreshed login.
// No login with an empty access token is ever written into the home.
func TestARefreshOnlyClaudeAccountIsRefreshedBeforeItRuns(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := refreshOnlyClaude(s, "u1")

	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed once, and run on the new token: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	if raw := readFile(filepath.Join(storeDir(s), "accounts.json")); !strings.Contains(raw, "A-new") || !strings.Contains(raw, "R-new") {
		t.Fatalf("the refresh is on disk before the run: %s", raw)
	}
	noEmptyAccessToken(t, s.Home(a))

	env, err = launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || f.refreshes.Load() != 1 {
		t.Fatalf("the next run takes the stored route, with no refresh: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	if r, doc := readLogin(t, s.Home(a)); r != "R-new" || doc["claudeAiOauth"].(map[string]any)["accessToken"] != "A-new" {
		t.Fatalf("the home holds the refreshed login: %v", doc)
	}
}

// One held as a refresh token alone with an expiry still ahead, which a store
// written by hand may carry, had that expiry believed: its stored route wrote a
// login with an empty access token into its home. It is refreshed first now,
// in its quiet home, saved, and the home is given the new token.
func TestARefreshOnlyClaudeAccountWithAnExpiryAheadIsRefreshedBeforeItsLoginIsWritten(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := refreshOnlyClaude(s, "u1")
	a.Token.ExpiresAt = later(time.Hour)
	ob.watch(s, a, "R-new")
	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed once, and run on its stored login: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	ob.savedThenWritten(t)
	noEmptyAccessToken(t, s.Home(a))
	if _, doc := readLogin(t, s.Home(a)); doc["claudeAiOauth"].(map[string]any)["accessToken"] != "A-new" {
		t.Fatalf("the home holds the new access token: %v", doc)
	}
}

// A listing refreshes a claude account held as a refresh token alone, in a
// quiet home, saves it before it writes the login into the home, and reads its
// usage with the new token, where it used to ask nothing and say the account
// had no access token. A refresh token the provider refuses ends the lineage
// with the provider's own words, and no usage is asked.
func TestAListingRefreshesARefreshOnlyClaudeAccount(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	var mu sync.Mutex
	var asked []string
	f.set(func(f *anthropic) {
		f.refresh = refreshesTo("A-new", "R-new")
		f.usage = func(auth string) (int, any) {
			mu.Lock()
			defer mu.Unlock()
			asked = append(asked, auth)
			return 200, map[string]any{"five_hour": map[string]any{"utilization": 10}}
		}
	})

	s, ob := orderedStore(t)
	a := refreshOnlyClaude(s, "u1")
	ob.watch(s, a, "R-new")
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	ob.savedThenWritten(t)
	noEmptyAccessToken(t, s.Home(a))
	mu.Lock()
	got := slices.Clone(asked)
	mu.Unlock()
	if f.refreshes.Load() != 1 || len(got) != 1 || got[0] != "Bearer A-new" || a.Quota == nil {
		t.Fatalf("refreshed once, and the usage read with the new token: %d %q %+v", f.refreshes.Load(), got, a.Quota)
	}

	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) {
			return 400, map[string]string{"error": "invalid_grant", "error_description": "refresh token not found"}
		}
	})
	f.refreshes.Store(0)
	f.usages.Store(0)
	s = openTemp(t)
	b := refreshOnlyClaude(s, "u2")
	errs := s.Refresh(context.Background(), true, b)
	if len(errs) != 1 || !errors.Is(errs[0], rota.ErrReauth) || !b.Dead || !strings.Contains(b.DeadReason, "invalid_grant") {
		t.Fatalf("refused: dead, with the provider's words: %v %v %q", errs, b.Dead, b.DeadReason)
	}
	if f.refreshes.Load() != 1 || f.usages.Load() != 0 {
		t.Fatalf("presented once, and no usage asked: %d %d", f.refreshes.Load(), f.usages.Load())
	}
	noEmptyAccessToken(t, s.Home(b))
}

// The server's sweep refreshes a claude account held as a refresh token alone
// and saves it; the next launch writes the login into the home.
func TestMaintenanceRefreshesARefreshOnlyClaudeAccount(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := refreshOnlyClaude(s, "u1")
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 || a.Token.Access != "A-new" || a.Token.Refresh != "R-new" || a.Expired() {
		t.Fatalf("refreshed: %d %+v", f.refreshes.Load(), a.Token)
	}
	if raw := readFile(filepath.Join(storeDir(s), "accounts.json")); !strings.Contains(raw, "A-new") {
		t.Fatalf("and saved: %s", raw)
	}
	noEmptyAccessToken(t, s.Home(a))
}

// A hermetic run of a claude account held as a refresh token alone refreshes
// it first, in a quiet home, and runs on the new token.
func TestAHermeticRunOfARefreshOnlyClaudeAccountRefreshesItFirst(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := refreshOnlyClaude(s, "u1")
	cmd, release, err := s.prepare(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed once, and run on the new token: %d %q", f.refreshes.Load(), tokenVars(cmd.Env))
	}
	noEmptyAccessToken(t, s.Home(a))
}

// A codex account held as a refresh token alone is refreshed before its
// auth.json is written, so the file carries the new access token and the
// refresh token that came with it, never an empty one. The id_token is
// already known, so the staging's own repair has nothing to do: one refresh.
func TestACodexAccountHeldAsARefreshTokenAloneIsRefreshedBeforeItsFileIsWritten(t *testing.T) {
	var mu sync.Mutex
	var forms []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "R1" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "A2", "refresh_token": "R2", "id_token": "idt-2", "expires_in": 864000})
	}))
	t.Cleanup(srv.Close)
	old := rota.CodexEndpoints.Token
	rota.CodexEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.CodexEndpoints.Token = old })

	s := openTemp(t)
	a := s.add("codex")
	a.Token = rota.Token{Refresh: "R1"}
	a.Extra = map[string]string{"id_token": "idt-1", "account_id": "acct"}
	a.StagedSuperseded()
	_, release, err := s.prepare(context.Background(), a, true)
	if err != nil {
		t.Fatal(err)
	}
	release()
	mu.Lock()
	n := len(forms)
	mu.Unlock()
	var doc struct {
		Tokens struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
		} `json:"tokens"`
	}
	raw, err := os.ReadFile(filepath.Join(s.Home(a), "auth.json"))
	if err != nil || json.Unmarshal(raw, &doc) != nil {
		t.Fatalf("auth.json: %v %s", err, raw)
	}
	if n != 1 || doc.Tokens.Access != "A2" || doc.Tokens.Refresh != "R2" || a.Token.Access != "A2" {
		t.Fatalf("refreshed once, and the file carries the new tokens: %d %s", n, raw)
	}
}
