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
// whose access token is empty. A store with no login in it holds no such thing.
func noEmptyAccessToken(t *testing.T, home string) {
	t.Helper()
	_, doc := readLogin(t, home)
	l, ok := doc["claudeAiOauth"].(map[string]any)
	if !ok {
		return
	}
	if access, _ := l["accessToken"].(string); access == "" {
		t.Fatalf("a login with an empty access token was written into the home: %v", doc)
	}
}

// accessIn is the access token of the login a credential store holds.
func accessIn(doc map[string]any) string {
	l, _ := doc["claudeAiOauth"].(map[string]any)
	access, _ := l["accessToken"].(string)
	return access
}

// tokenVars is what of an environment hands Claude Code a token, for a
// failure to show without the rest of the machine's variables.
func tokenVars(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(e string) bool { return !strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") })
}

// A claude account held as a refresh token alone is refreshed before its
// first run in its quiet home, the way an expired login there is: saved first,
// written into the home after, and the run takes the stored route on it, as
// every login does. A second run while the first one's window is open joins
// that login, with no refresh and no word of Claude Code from before. No login
// with an empty access token is ever written into the home.
func TestARefreshOnlyClaudeAccountIsRefreshedBeforeItRuns(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	words := said(s)
	a := refreshOnlyClaude(s, "u1")
	ob.watch(s, a, "R-new")

	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed once, and run on its stored login: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	if got := configDirs(env); len(got) != 1 || got[0] != s.Home(a) || a.Extra[routeKey] != routeStored {
		t.Fatalf("pointed at its home, the stored route remembered: %v %q", got, a.Extra[routeKey])
	}
	ob.savedThenWritten(t)
	if r, doc := readLogin(t, s.Home(a)); r != "R-new" || accessIn(doc) != "A-new" {
		t.Fatalf("the home holds the refreshed login: %v", doc)
	}

	alive(t, s.Home(a), "sessions/1.json", os.Getpid()) // the first run's window
	env, err = launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || f.refreshes.Load() != 1 {
		t.Fatalf("the next run joins that login, with no refresh: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	if slices.ContainsFunc(*words, func(w string) bool { return strings.Contains(w, "from before") }) {
		t.Fatalf("nothing in the home is from before: %q", *words)
	}
	noEmptyAccessToken(t, s.Home(a))
}

// While anything of Claude Code's that may hold its refresh token runs in its
// home, a claude account held as a refresh token alone is refreshed by
// nothing, exactly as an expired login there is not: not by a run, a hermetic
// run, a listing or the server's sweep, and nothing is written into the home.
// A home holding the account's own refresh token is joined as it is; one that
// shows no login with a window alive on the stored route, or that cannot be
// read, refuses the run as busy; and so does a home Claude Code blanked.
func TestARefreshOnlyClaudeAccountIsNotRefreshedWhileItsHomeMayHoldItsToken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	hermetic := func(s *Store, a *rota.Account) error {
		_, release, err := s.prepare(context.Background(), a, false)
		if err == nil {
			release()
		}
		return err
	}
	// untouched lists and sweeps the account too, and finds nothing presented,
	// nothing taken and nothing written.
	untouched := func(t *testing.T, s *Store, a *rota.Account, store string) {
		t.Helper()
		s.Refresh(context.Background(), true, a)
		s.Maintain(context.Background())
		if f.refreshes.Load() != 0 || a.Token.Access != "" || a.Token.Refresh != "R-u1" {
			t.Fatalf("nothing presented its refresh token: %d %+v", f.refreshes.Load(), a.Token)
		}
		if got := readFile(filepath.Join(s.Home(a), ".credentials.json")); got != store {
			t.Fatalf("nothing written into the home: %q", got)
		}
	}
	busy := func(t *testing.T, s *Store, a *rota.Account) {
		t.Helper()
		if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrBusy) {
			t.Fatalf("a run is refused as busy: %v", err)
		}
		if err := hermetic(s, a); !errors.Is(err, rota.ErrBusy) {
			t.Fatalf("a hermetic run too: %v", err)
		}
	}

	t.Run("in the hold", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		a := refreshOnlyClaude(s, "u1")
		torn := `{"claudeAiOauth":{"refreshToken":"R-cc"`
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), torn)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		busy(t, s, a)
		untouched(t, s, a, torn)
	})
	t.Run("a window on its stored route and no login in the home", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		a := refreshOnlyClaude(s, "u1")
		a.Extra = map[string]string{routeKey: routeStored}
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		busy(t, s, a)
		untouched(t, s, a, "")
	})
	t.Run("a Claude Code holding its own refresh token", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		a := refreshOnlyClaude(s, "u1")
		held := `{"claudeAiOauth":{"accessToken":"","refreshToken":"R-u1","expiresAt":1999999999000,"scopes":["user:inference"]}}`
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), held)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		env, err := launchEnv(t, s, a)
		if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("joined as it is: %v %q", err, tokenVars(env))
		}
		if got := configDirs(env); len(got) != 1 || got[0] != s.Home(a) {
			t.Fatalf("pointed at its home: %v", got)
		}
		if err := hermetic(s, a); !errors.Is(err, rota.ErrBusy) {
			t.Fatalf("a hermetic run cannot be handed a token while that runs: %v", err)
		}
		untouched(t, s, a, held)
	})
	t.Run("a home Claude Code blanked", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		a := refreshOnlyClaude(s, "u1")
		a.StagedWritten()
		blank := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), blank)
		busy(t, s, a)
		untouched(t, s, a, blank)
	})
}

// Beside windows that were never on its stored login and hold none, the
// account's refresh token lives in rota's store alone, so a run refreshes it
// and goes on a token, exactly as it does for an expired login in that home.
func TestARefreshOnlyClaudeAccountBesideWindowsThatHoldNoLoginRunsOnAToken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := refreshOnlyClaude(s, "u1")
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed once, and run on the new token: %v %d %q", err, f.refreshes.Load(), tokenVars(env))
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("nothing written into a home where something runs")
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
// the way it refreshes an expired login in a quiet home: under the idle
// claim, saved, and then written into the home.
func TestMaintenanceRefreshesARefreshOnlyClaudeAccount(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := refreshOnlyClaude(s, "u1")
	ob.watch(s, a, "R-new")
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 || a.Token.Access != "A-new" || a.Token.Refresh != "R-new" || a.Expired() {
		t.Fatalf("refreshed: %d %+v", f.refreshes.Load(), a.Token)
	}
	ob.savedThenWritten(t)
	noEmptyAccessToken(t, s.Home(a))
	if s.claimed(a) {
		t.Fatal("the claim the sweep took is let go")
	}
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
