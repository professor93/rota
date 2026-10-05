package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/professor93/rota/internal/claudecode"
	rota "github.com/professor93/rota/lib"
)

// deadPID names no process on any machine these tests run on.
const deadPID = 2147483000

// livingClaude adds a claude account whose login Claude Code can keep: a
// refresh token, an access token good for an hour, and the refresh token's
// own expiry, which is what tells a rotation of this login from a new one.
func livingClaude(s *Store, uuid string) *rota.Account {
	a := s.add("claude")
	a.UUID, a.Email = uuid, uuid+"@x"
	a.Token = rota.Token{Access: "A-" + uuid, Refresh: "R-" + uuid, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		Scopes: []string{"user:inference"}}
	a.Extra = map[string]string{"refresh_token_expires_at": "1999999999000", "subscription_type": "max", "rate_limit_tier": "t20"}
	a.StagedSuperseded()
	return a
}

// expire makes the account's access token one that must be refreshed.
func expire(a *rota.Account) { a.Token.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli() }

// storeLogin is a credential store holding one login, as Claude Code writes
// it, with something of Claude Code's own beside it.
func storeLogin(access, refresh string, expires int64, until string) string {
	b, _ := json.Marshal(map[string]any{
		"mcpOAuth": map[string]any{"srv": map[string]string{"accessToken": "mcp-secret"}},
		"claudeAiOauth": map[string]any{"accessToken": access, "refreshToken": refresh, "expiresAt": expires,
			"refreshTokenExpiresAt": json.RawMessage(until), "scopes": []string{"user:inference"}},
	})
	return string(b)
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// alive writes a liveness record naming pid, as Claude Code does.
func alive(t *testing.T, home, name string, pid int) {
	t.Helper()
	writeFile(t, filepath.Join(home, name), `{"pid":`+itoa(pid)+`,"kind":"interactive","status":"busy"}`)
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// readLogin is the login in a home's credential file.
func readLogin(t *testing.T, home string) (refresh string, doc map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if err != nil {
		return "", nil
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	l, _ := doc["claudeAiOauth"].(map[string]any)
	r, _ := l["refreshToken"].(string)
	return r, doc
}

// keychain stands in for the macOS keychain, and remembers every question
// security was asked.
type keychain struct {
	mu       sync.Mutex
	items    map[string]string
	log      []string
	onDelete func(service string)
}

func fakeKeychain(t *testing.T) *keychain {
	t.Helper()
	k := &keychain{items: map[string]string{}}
	oldKept := keychainKept
	keychainKept = true
	claudecode.Security = func(_ context.Context, args ...string) ([]byte, int, error) {
		k.mu.Lock()
		defer k.mu.Unlock()
		svc := ""
		for i, a := range args {
			if a == "-s" && i+1 < len(args) {
				svc = args[i+1]
			}
		}
		k.log = append(k.log, args[0]+" "+svc)
		switch args[0] {
		case "find-generic-password":
			if v, ok := k.items[svc]; ok {
				return []byte(v + "\n"), 0, nil
			}
			return nil, claudecode.Absent, nil
		case "delete-generic-password":
			if k.onDelete != nil {
				k.onDelete(svc)
			}
			if _, ok := k.items[svc]; ok {
				delete(k.items, svc)
				return nil, 0, nil
			}
			return nil, claudecode.Absent, nil
		}
		t.Errorf("rota must never ask security for %v", args)
		return nil, 1, nil
	}
	t.Cleanup(func() {
		keychainKept = oldKept
		claudecode.StandIn()
	})
	return k
}

func (k *keychain) asked() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.log)
}

func service(t *testing.T, home string) string {
	t.Helper()
	svc, ok := claudecode.Service(home)
	if !ok {
		t.Fatalf("%s has no service name", home)
	}
	return svc
}

// daemons stands in for `claude daemon stop --any`.
type daemons struct {
	mu    sync.Mutex
	homes []string
	envs  [][]string
	err   error
	then  func(home string)
}

func fakeDaemons(t *testing.T) *daemons {
	t.Helper()
	d := &daemons{}
	claudecode.StopDaemon = func(_ context.Context, home string, env []string) error {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.homes = append(d.homes, home)
		d.envs = append(d.envs, env)
		if d.then != nil {
			d.then(home)
		}
		return d.err
	}
	t.Cleanup(claudecode.StandIn)
	return d
}

// anthropic stands in for the provider: its token, profile and usage
// endpoints, each counted.
type anthropic struct {
	refreshes, profiles, usages atomic.Int64
	refresh                     func() (int, any)
	profile                     func(auth string) (int, any)
	usage                       func(auth string) (int, any)
}

func fakeAnthropic(t *testing.T) *anthropic {
	t.Helper()
	f := &anthropic{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status int
		var body any
		switch r.URL.Path {
		case "/token":
			f.refreshes.Add(1)
			status, body = 500, nil
			if f.refresh != nil {
				status, body = f.refresh()
			}
		case "/profile":
			f.profiles.Add(1)
			status, body = 500, nil
			if f.profile != nil {
				status, body = f.profile(r.Header.Get("Authorization"))
			}
		case "/usage":
			f.usages.Add(1)
			status, body = 200, map[string]any{"five_hour": map[string]any{"utilization": 10}}
			if f.usage != nil {
				status, body = f.usage(r.Header.Get("Authorization"))
			}
		}
		w.WriteHeader(status)
		if body != nil {
			json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	for _, e := range []struct {
		p   *string
		url string
	}{{&rota.ClaudeEndpoints.Token, srv.URL + "/token"}, {&rota.ClaudeEndpoints.Profile, srv.URL + "/profile"},
		{&rota.ClaudeEndpoints.Usage, srv.URL + "/usage"}} {
		old := *e.p
		*e.p = e.url
		t.Cleanup(func() { *e.p = old })
	}
	return f
}

// launchEnv stages a launch the way a handover does and returns the child's
// environment, with the claim already let go.
func launchEnv(t *testing.T, s *Store, a *rota.Account) ([]string, error) {
	t.Helper()
	cmd, release, err := s.prepare(context.Background(), a, true)
	if err != nil {
		return nil, err
	}
	release()
	return rota.Environ(HostEnv(), cmd), nil
}

func hasVar(env []string, name string) bool {
	return slices.ContainsFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
}

func storedRouteHere(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps the environment route")
	}
}

/* ------------------------------------------------------------ the route --- */

// A launch in the account's home runs on the account's own stored login:
// Claude Code is pointed at the home, exactly once, and nothing that
// authenticates reaches it — not rota's token, and not one the shell
// exported. The login is in the home's store, written there because the home
// held nothing yet.
func TestAClaudeLaunchRunsOnItsOwnStoredLoginWithNoTokenInItsEnvironment(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "from-the-shell")
	t.Setenv("ANTHROPIC_API_KEY", "a-key")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/somebody/else")
	s := openTemp(t)
	a := livingClaude(s, "u1")
	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		if hasVar(env, name) {
			t.Fatalf("%s must not reach the child: %v", name, env)
		}
	}
	for _, e := range env {
		if strings.Contains(e, "A-u1") || strings.Contains(e, "R-u1") {
			t.Fatalf("no token in the environment: %s", e)
		}
	}
	if got := configDirs(env); len(got) != 1 || got[0] != s.Home(a) {
		t.Fatalf("exactly one configuration directory, the home: %v", got)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
		t.Fatalf("the home holds the account's login: %q", r)
	}
	if a.Extra[routeKey] != routeStored {
		t.Fatal("the route is remembered")
	}
}

// A hermetic run has no home to keep a login in, and goes on a token in its
// environment.
func TestAHermeticRunGoesOnATokenInItsEnvironment(t *testing.T) {
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	cmd, release, err := s.prepare(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("the access token, in the environment: %v", cmd.Env)
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("and nothing is written for it: %v", err)
	}
}

// A hermetic run on an expired token needs a refresh, which a home with
// Claude Code alive in it cannot give. It is refused, and told what such
// runs are for; with the home quiet the refresh happens and the new login
// goes into the home at once.
func TestAHermeticRunOnAnExpiredTokenIsRefusedWhileTheHomeIsAlive(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.refresh = func() (int, any) {
		return 200, map[string]any{"access_token": "A-new", "refresh_token": "R-new", "expires_in": 3600}
	}
	s := openTemp(t)
	a := livingClaude(s, "u1")
	expire(a)
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	_, _, err := s.prepare(context.Background(), a, false)
	if !errors.Is(err, rota.ErrBusy) || !strings.Contains(err.Error(), "rota login --long") {
		t.Fatalf("refused, and told why: %v", err)
	}
	if f.refreshes.Load() != 0 {
		t.Fatal("nothing may refresh a login a running Claude Code holds")
	}
	os.Remove(filepath.Join(s.Home(a), "sessions", "1.json"))
	cmd, release, err := s.prepare(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") {
		t.Fatalf("the refreshed token: %v", cmd.Env)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-new" {
		t.Fatalf("and the new login is in the home at once: %q", r)
	}
}

/* ------------------------------------------------------------ the claim --- */

// Many runs on one claude account at once are the point: the claim is
// shared, the rotation never sees such an account as busy, and a codex
// account is still one run at a time.
func TestTwoRunsOfAClaudeAccountGoAtOnceAndACodexAccountStillDoesNot(t *testing.T) {
	s := openTemp(t)
	a := livingClaude(s, "u1")
	first, ok := s.holdForExec(a)
	if !ok {
		t.Fatal("first")
	}
	defer first()
	second, ok := s.holdForExec(a)
	if !ok {
		t.Fatal("a second run of a claude account must go ahead")
	}
	defer second()
	if s.Busy(a) {
		t.Fatal("the rotation must never pass a claude account over because it runs")
	}
	if _, idle := s.holdIdle(a); idle {
		t.Fatal("but it is not idle")
	}
	if err := s.Removable(a); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("and it cannot be removed under a run: %v", err)
	}
	c := s.add("codex")
	release, ok := s.holdForExec(c)
	if !ok {
		t.Fatal("codex first")
	}
	defer release()
	if _, ok := s.holdForExec(c); ok || !s.Busy(c) {
		t.Fatal("a codex account runs once at a time")
	}
}

/* ---------------------------------------------------------- what is alive --- */

// Alive is what Claude Code's own records say: a daemon lock or a session
// record naming a live pid, or a record nobody can read. A record naming a
// pid that is gone is a leftover.
func TestAHomeIsAliveByTheRecordsClaudeCodeKeepsThere(t *testing.T) {
	for _, c := range []struct {
		what, name, body string
		alive            bool
	}{
		{"a daemon", "daemon.lock", `{"pid":` + itoa(os.Getpid()) + `,"procStart":"x"}`, true},
		{"a session", "sessions/123.json", `{"pid":` + itoa(os.Getpid()) + `,"kind":"bg"}`, true},
		{"an unreadable record", "sessions/9.json", `{"pid":`, true},
		{"a record with no pid", "daemon.lock", `{}`, true},
		{"a dead daemon", "daemon.lock", `{"pid":` + itoa(deadPID) + `}`, false},
		{"a dead session", "sessions/7.json", `{"pid":` + itoa(deadPID) + `}`, false},
		{"something else in sessions", "sessions/key.bin", `x`, false},
	} {
		home := t.TempDir()
		writeFile(t, filepath.Join(home, c.name), c.body)
		if got := len(claudeLive(home)) > 0; got != c.alive {
			t.Fatalf("%s: alive=%v %v", c.what, got, claudeLive(home))
		}
	}
	if len(claudeLive(t.TempDir())) != 0 || len(claudeLive(filepath.Join(t.TempDir(), "none"))) != 0 {
		t.Fatal("an empty home, or none, is quiet")
	}
}

/* ------------------------------------------------------------ adoption --- */

// A rotation by Claude Code keeps the refresh token's own expiry, and is
// taken from the home with no network call at all.
func TestARotationByClaudeCodeIsTakenWithoutAskingAnybody(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(3 * time.Hour).UnixMilli()
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-rot", "R-rot", later, "1999999999000"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-rot" || a.Token.Access != "A-rot" || a.Token.ExpiresAt != later {
		t.Fatalf("the rotation is the account's now: %+v", a.Token)
	}
	if n := f.profiles.Load() + f.refreshes.Load() + f.usages.Load(); n != 0 {
		t.Fatalf("a plain rotation asks nobody anything: %d calls", n)
	}
}

// A login somebody made with /login inside the home is a new login, and it
// is checked with the provider before anything of it is taken: the same
// account's is taken — a dead account comes back with it — and somebody
// else's is refused with the account's own tokens left exactly as they
// were. When the provider cannot be asked, nothing changes.
func TestANewLoginInTheHomeIsCheckedBeforeItIsTaken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	whose := "u1"
	f.profile = func(auth string) (int, any) {
		if auth != "Bearer A-login" {
			return 401, nil
		}
		return 200, map[string]any{"account": map[string]string{"uuid": whose, "email": whose + "@x"}}
	}
	later := time.Now().Add(5 * time.Hour).UnixMilli()
	newLogin := storeLogin("A-login", "R-login", later, "1777777777000")

	t.Run("this account's", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.Dead, a.DeadReason = true, "invalid_grant"
		a.Staged = ""
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), newLogin)
		whose = "u1"
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		if a.Dead || a.Token.Refresh != "R-login" || a.Extra["refresh_token_expires_at"] != "1777777777000" {
			t.Fatalf("somebody signed it in again inside Claude Code: %+v", a)
		}
	})
	t.Run("somebody else's", func(t *testing.T) {
		s := openTemp(t)
		var said []string
		s.Warn = func(m string) { said = append(said, m) }
		a := livingClaude(s, "u1")
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", a.Token.ExpiresAt, "1999999999000"))
		a.Staged = ""
		h := s.claudeHome(a, false)
		if err := h.adopt(context.Background()); err != nil || a.Staged == "-" {
			t.Fatalf("first, in sync: %v %q", err, a.Staged)
		}
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), newLogin)
		whose = "u9"
		before := a.Token
		if err := h.adopt(context.Background()); err != nil {
			t.Fatal(err)
		}
		if a.Token.Refresh != before.Refresh || a.Token.Access != before.Access || a.Staged != "-" {
			t.Fatalf("not taken, and the home is marked for the account's own login: %+v %q", a.Token, a.Staged)
		}
		if len(said) != 1 || !strings.Contains(said[0], "u9@x") || !strings.Contains(said[0], "u1@x") {
			t.Fatalf("said, naming both: %q", said)
		}
	})
	t.Run("nobody can be asked", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.Staged = "abc"
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), newLogin)
		f.profile = func(string) (int, any) { return 503, nil }
		before := *a
		if err := s.claudeHome(a, false).adopt(context.Background()); err != nil {
			t.Fatal(err)
		}
		if a.Token.Refresh != before.Token.Refresh || a.Staged != before.Staged || a.Dead {
			t.Fatalf("nothing changes until the provider can say: %+v", a)
		}
	})
}

// On macOS the keychain item is the store Claude Code reads, before the file
// beside it, and adoption reads it the same way.
func TestTheKeychainItemIsReadBeforeTheFile(t *testing.T) {
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.Staged = ""
	later := time.Now().Add(4 * time.Hour).UnixMilli()
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-file", "R-file", later, "1999999999000"))
	k.items[service(t, s.Home(a))] = storeLogin("A-key", "R-key", later, "1999999999000")
	if err := s.claudeHome(a, false).adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-key" {
		t.Fatalf("the keychain wins: %+v", a.Token)
	}
}

/* ------------------------------------------------------------- staging --- */

// Writing the login keeps every other secret Claude Code keeps in the store,
// lands it privately and in one piece, and is recorded and saved.
func TestSeedingAHomeKeepsItsOtherSecretsAndIsSaved(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"),
		`{"mcpOAuth":{"srv":{"accessToken":"mcp-secret"}},"trustedDeviceToken":"tdt","claudeAiOauth":{"refreshToken":"R-old","expiresAt":1,"refreshTokenExpiresAt":5}}`)
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	r, doc := readLogin(t, s.Home(a))
	if r != "R-u1" || doc["trustedDeviceToken"] != "tdt" || doc["mcpOAuth"] == nil {
		t.Fatalf("the login replaced, the rest kept: %v", doc)
	}
	l := doc["claudeAiOauth"].(map[string]any)
	if l["subscriptionType"] != "max" || l["rateLimitTier"] != "t20" || l["refreshTokenExpiresAt"] != float64(1999999999000) {
		t.Fatalf("the plan goes with it, so Claude Code has no reason to rewrite the shared identity: %v", l)
	}
	fi, _ := os.Stat(filepath.Join(s.Home(a), ".credentials.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(s.Home(a), "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("written whole, through a rename: %v", leftovers)
	}
	s.Close()
	s2, err := Open(storeDir(s))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Find(a.ID); got.Staged == "-" || got.Staged == "" {
		t.Fatalf("what the home holds is on disk: %q", got.Staged)
	}
}

// A home already holding the account's login is not written again.
func TestAHomeHoldingTheLoginIsNotWrittenAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Home(a), ".credentials.json")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Fatal("a live Claude Code notices a write by its time; there must not be one")
	}
}

// A write rota meant to make and never made — rota stopped between saving
// and writing — is made by the next launch, because what the account
// records about the home no longer matches.
func TestAWriteThatNeverHappenedIsMadeByTheNextLaunch(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", a.Token.ExpiresAt, "1999999999000"))
	a.StagedWritten() // the home held R-u1, as rota wrote it
	// rota refreshed, saved R-newer, and stopped before the write.
	a.Token.Access, a.Token.Refresh = "A-newer", "R-newer"
	a.Token.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-newer" {
		t.Fatalf("written now: %q", r)
	}
}

// With Claude Code alive in the home and a login there it can use, nothing
// is written: the launch joins what is there, and the person is told when
// the account's current login takes over.
func TestAnAliveHomeOnAnEarlierLoginIsJoinedAndNotWritten(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	a := livingClaude(s, "u1")
	earlier := storeLogin("A-old", "R-old", time.Now().Add(time.Hour).UnixMilli(), "1999999999000")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), earlier)
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	a.Extra[routeKey] = routeStored
	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatal(err)
	}
	if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatal("the stored route still")
	}
	if raw, _ := os.ReadFile(filepath.Join(s.Home(a), ".credentials.json")); string(raw) != earlier {
		t.Fatal("nothing may be written while Claude Code is alive there")
	}
	if len(said) != 1 || !strings.Contains(said[0], "/login") {
		t.Fatalf("and the person is told: %q", said)
	}
}

// With Claude Code alive in the home and no usable login there — windows
// from before this rota, on tokens in their environment — this launch goes
// the same way rather than start a Claude Code that is not signed in; and
// with its own token expired it is refused instead.
func TestAnAliveHomeWithNoLoginLaunchesOnATokenAndNeverLoggedOut(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	a := livingClaude(s, "u1")
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("on a token: %v", env)
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("and nothing written")
	}
	if len(said) == 0 || !strings.Contains(said[len(said)-1], "without Remote Control") {
		t.Fatalf("said: %q", said)
	}
	expire(a)
	if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("an expired token nobody may refresh is refused: %v", err)
	}
}

/* --------------------------------------------------------------- macOS --- */

// On macOS the keychain item goes first and the file is written after, and
// nothing is ever written into the keychain.
func TestSeedingOnMacOSDeletesTheKeychainItemAndThenWritesTheFile(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	svc := service(t, s.Home(a))
	k.items[svc] = `{"mcpOAuth":{"from":"keychain"},"claudeAiOauth":{"refreshToken":"R-old","expiresAt":1,"refreshTokenExpiresAt":1999999999000}}`
	fileAtDelete := true
	k.onDelete = func(string) {
		_, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json"))
		fileAtDelete = err == nil
	}
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if fileAtDelete {
		t.Fatal("the item goes before the file is written")
	}
	if _, ok := k.items[svc]; ok {
		t.Fatal("the item is gone")
	}
	r, doc := readLogin(t, s.Home(a))
	if r != "R-u1" || doc["mcpOAuth"] == nil {
		t.Fatalf("the file holds the login and what the item held beside it: %v", doc)
	}
}

// A file beside a keychain item is stale — Claude Code reads the item — and
// is removed when nothing runs in the home.
func TestAStaleFileBesideAKeychainItemIsRemoved(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.Staged = ""
	k.items[service(t, s.Home(a))] = storeLogin("A-u1", "R-u1", a.Token.ExpiresAt, "1999999999000")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-stale", "R-stale", 1, "1999999999000"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("the stale file is gone: %v", err)
	}
}

// A home whose path is not plain ASCII has no keychain name rota can work
// out: nothing is asked of the keychain, the file is used, and the person is
// told once.
func TestAHomeWithANonASCIIPathUsesTheFileAloneAndSaysSo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	a := livingClaude(s, "u1")
	a.ConfigDir = filepath.Join(t.TempDir(), "projét")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if got := k.asked(); len(got) != 0 {
		t.Fatalf("nothing asked of the keychain: %v", got)
	}
	if r, _ := readLogin(t, a.ConfigDir); r != "R-u1" {
		t.Fatal("the file is used")
	}
	n := 0
	for _, m := range said {
		if strings.Contains(m, "ASCII") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("said once: %q", said)
	}
}

/* ---------------------------------------------------------- refreshing --- */

// rota refreshes a login kept in the home only while nothing runs there,
// and writes the new login into the home at once.
func TestMaintenanceRefreshesOnlyAQuietHomeAndWritesTheNewLoginIntoIt(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.refresh = func() (int, any) {
		return 200, map[string]any{"access_token": "A-new", "refresh_token": "R-new", "expires_in": 3600}
	}
	s := openTemp(t)
	a := livingClaude(s, "u1")
	expire(a)
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	s.Maintain(context.Background())
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatal("not while Claude Code holds the login")
	}
	os.Remove(filepath.Join(s.Home(a), "sessions", "1.json"))
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 || a.Token.Refresh != "R-new" {
		t.Fatalf("refreshed: %d %+v", f.refreshes.Load(), a.Token)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-new" {
		t.Fatalf("and the home holds it at once: %q", r)
	}
}

// Usage is read with the token the account has, while Claude Code runs or
// not. A 401 is Claude Code having rotated the login meanwhile: the home is
// read again and the reading tried once more with what is there.
func TestUsageIsReadWhileAliveAndRetriedOnceAfterA401(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	var seen []string
	f.usage = func(auth string) (int, any) {
		seen = append(seen, auth)
		if auth != "Bearer A-rot" {
			return 401, nil
		}
		return 200, map[string]any{"five_hour": map[string]any{"utilization": 42}}
	}
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.Staged = ""
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", a.Token.ExpiresAt, "1999999999000"))
	if err := s.claudeHome(a, true).adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Claude Code rotates between the reading of the home and the request.
	h := s.claudeHome(a, true)
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"),
		storeLogin("A-rot", "R-rot", time.Now().Add(2*time.Hour).UnixMilli(), "1999999999000"))
	q, err := h.usage(context.Background())
	if err != nil || len(q.Windows) == 0 || q.Windows[0].Percent != 42 {
		t.Fatalf("%+v %v", q, err)
	}
	if !slices.Equal(seen, []string{"Bearer A-u1", "Bearer A-rot"}) {
		t.Fatalf("once with the old token, once with the rotated one: %v", seen)
	}
	if f.refreshes.Load() != 0 {
		t.Fatal("and nothing was refreshed")
	}
}

// An expired token in a home where Claude Code runs is left alone: the
// account keeps its last reading, and that is not an error.
func TestARunningAccountWithAnExpiredTokenKeepsItsLastReading(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	expire(a)
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load()+f.usages.Load() != 0 {
		t.Fatal("nothing asked")
	}
}

// When the provider refuses rota's own refresh as dead, the home is read
// again before that is believed: a sibling may have refreshed first, and
// the login it left there is this account's, alive.
func TestARefusedRefreshIsNotBelievedUntilTheHomeIsReadAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	expire(a)
	home := s.Home(a)
	f.refresh = func() (int, any) {
		// The sibling got there first: the home holds its login now.
		writeFile(t, filepath.Join(home, ".credentials.json"),
			storeLogin("A-sib", "R-sib", time.Now().Add(time.Hour).UnixMilli(), "1999999999000"))
		return 400, map[string]string{"error": "invalid_grant"}
	}
	f.profile = func(auth string) (int, any) {
		return 200, map[string]any{"account": map[string]string{"uuid": "u1"}}
	}
	h := s.claudeHome(a, false)
	if _, err := h.renew(context.Background()); err != nil {
		t.Fatalf("the login is alive: %v", err)
	}
	if a.Dead || a.Token.Refresh != "R-sib" {
		t.Fatalf("and it is the sibling's: %+v", a)
	}
}

/* --------------------------------------------- the daemon, and removing --- */

// A launch on another route than the home's daemon was started on stops
// that daemon, with nothing in its environment that authenticates, and says
// so; a daemon on the same route is left alone, and one that will not stop
// is said too and does not stop the launch.
func TestChangingRouteRetiresTheDaemonStartedOnTheOther(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	d := fakeDaemons(t)
	d.then = func(home string) { os.Remove(filepath.Join(home, "daemon.lock")) }
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "from-the-shell")
	s := openTemp(t)
	var said []string
	s.Warn = func(m string) { said = append(said, m) }
	a := livingClaude(s, "u1")
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if len(d.homes) != 1 || d.homes[0] != s.Home(a) {
		t.Fatalf("stopped once, in the account's home: %v", d.homes)
	}
	env := d.envs[0]
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR="+s.Home(a)) || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("pointed at the home, and not authenticated: %v", env)
	}
	if len(said) == 0 || !strings.Contains(said[0], "stopped the Claude Code daemon") {
		t.Fatalf("said: %q", said)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
		t.Fatal("with the daemon gone the home is quiet, and the login goes in")
	}
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if len(d.homes) != 1 {
		t.Fatal("a daemon on the same route is left alone")
	}
	d.err, d.then = errors.New("no"), nil
	a.Extra[routeKey] = routeEnv
	said = nil
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatalf("a daemon that will not stop does not stop the launch: %v", err)
	}
	if len(said) == 0 || !strings.Contains(said[0], "could not be stopped") {
		t.Fatalf("said: %q", said)
	}
}

// Removing a claude account stops its daemon first, waits for the home to go
// quiet, and takes the keychain item before the directory.
func TestRemovingAClaudeAccountStopsItsDaemonAndTakesTheKeychainItemFirst(t *testing.T) {
	claudeWorld(t)
	k := fakeKeychain(t)
	d := fakeDaemons(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	svc := service(t, home)
	k.items[svc] = "{}"
	alive(t, home, "daemon.lock", os.Getpid())
	d.then = func(h string) { os.Remove(filepath.Join(h, "daemon.lock")) }
	homeAtDelete := false
	k.onDelete = func(string) {
		_, err := os.Stat(home)
		homeAtDelete = err == nil
	}
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if len(d.homes) != 1 || !homeAtDelete {
		t.Fatalf("daemon stopped %v; the item went while the home was still there: %v", d.homes, homeAtDelete)
	}
	if _, ok := k.items[svc]; ok {
		t.Fatal("the item is gone")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("and the home")
	}
}

// A home that does not go quiet — a window somebody opened there — stops
// the removal, naming what is alive.
func TestRemovingAnAccountStillAliveIsRefusedWithWhatIsAlive(t *testing.T) {
	claudeWorld(t)
	fakeDaemons(t)
	old := removalWait
	removalWait = 50 * time.Millisecond
	t.Cleanup(func() { removalWait = old })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	alive(t, s.Home(a), "sessions/77.json", os.Getpid())
	err := s.Remove(a.ID)
	if !errors.Is(err, rota.ErrBusy) || !strings.Contains(err.Error(), itoa(os.Getpid())) {
		t.Fatalf("refused, with the pid: %v", err)
	}
	if s.Find(a.ID) == nil {
		t.Fatal("and still there")
	}
}

// In a directory the person chose, only the account's login goes.
func TestRemovingAnAccountFromAChosenDirectoryTakesOnlyItsLogin(t *testing.T) {
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.ConfigDir = t.TempDir()
	writeFile(t, filepath.Join(a.ConfigDir, ".credentials.json"), "{}")
	writeFile(t, filepath.Join(a.ConfigDir, "CLAUDE.md"), "mine")
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.ConfigDir, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("the login goes")
	}
	if _, err := os.Stat(filepath.Join(a.ConfigDir, "CLAUDE.md")); err != nil {
		t.Fatal("the rest stays")
	}
}
