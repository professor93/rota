package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

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

// A hermetic run on an expired token needs a refresh. It is refused while a
// Claude Code in the home holds the account's login — refreshing would spend
// the token under it — and told what such runs are for. When what runs
// there holds no login of its own, the account's refresh token lives only in
// rota's store and is refreshed as it always was. And with the home quiet the
// refresh happens and the new login goes into the home after it is saved.
func TestAHermeticRunOnAnExpiredTokenRefreshesOnlyWhatNoClaudeCodeHolds(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	hermetic := func(s *Store, a *rota.Account) (*rota.Command, error) {
		cmd, release, err := s.prepare(context.Background(), a, false)
		if err == nil {
			release()
		}
		return cmd, err
	}

	t.Run("held by a running Claude Code", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		// The home holds the account's login, its access token lapsed too.
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", later(-time.Hour), "1999999999000"))
		expire(a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		_, err := hermetic(s, a)
		if !errors.Is(err, rota.ErrBusy) || !strings.Contains(err.Error(), "rota login --long") || f.refreshes.Load() != 0 {
			t.Fatalf("refused, told why, nothing refreshed: %v %d", err, f.refreshes.Load())
		}
	})
	t.Run("beside windows that hold no login", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		a := livingClaude(s, "u1")
		expire(a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		cmd, err := hermetic(s, a)
		if err != nil || !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") || f.refreshes.Load() != 1 {
			t.Fatalf("refreshed and run on the new token: %v %v", err, cmd)
		}
		if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
			t.Fatal("nothing written into a home where something runs")
		}
	})
	t.Run("a quiet home", func(t *testing.T) {
		s, ob := orderedStore(t)
		a := livingClaude(s, "u1")
		expire(a)
		ob.watch(s, a, "R-new")
		cmd, err := hermetic(s, a)
		if err != nil || !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") {
			t.Fatalf("%v %v", err, cmd)
		}
		ob.savedThenWritten(t)
	})
}

/* ------------------------------------------------------------ the claim --- */

// Many runs on one claude account at once are the point: two launches go
// through together, the rotation never sees the account as busy, nothing
// may refresh or remove it while they run, and the claim is gone when they
// end. A codex account is still one run at a time.
func TestTwoRunsOfAClaudeAccountGoAtOnceAndACodexAccountStillDoesNot(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	_, first, err := s.prepare(context.Background(), a, true)
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := s.prepare(context.Background(), a, true)
	if err != nil {
		t.Fatalf("a second launch of a claude account must go ahead: %v", err)
	}
	if s.Busy(a) {
		t.Fatal("the rotation must never pass a claude account over because it runs")
	}
	if _, idle := s.holdIdle(a); idle {
		t.Fatal("but it is not idle")
	}
	if err := s.Removable(a); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("and it cannot be removed under a run: %v", err)
	}
	first()
	if !s.claimed(a) {
		t.Fatal("one run still holds it")
	}
	second()
	if s.claimed(a) || s.Removable(a) != nil {
		t.Fatal("both over: nothing holds it")
	}
	c := s.add("codex")
	release, ok := s.holdRun(c)
	if !ok {
		t.Fatal("codex first")
	}
	defer release()
	if _, ok := s.holdRun(c); ok || !s.Busy(c) {
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

// Only the daemon and the sessions it hosts end with it. A window somebody
// has open, a record nobody can read, or another rota run outlives a stop,
// so their presence means the daemon is not alone.
func TestOnlyTheDaemonAndWhatItHostsEndWithIt(t *testing.T) {
	s := openTemp(t)
	a := livingClaude(s, "u1")
	h := s.claudeHome(a, false)
	writeFile(t, filepath.Join(h.home, "daemon.lock"), `{"pid":`+itoa(os.Getpid())+`}`)
	hosted(t, h.home, "sessions/1.json", os.Getpid())
	writeFile(t, filepath.Join(h.home, "sessions", "2.json"), `{"pid":`+itoa(os.Getpid())+`,"kind":"daemon-worker"}`)
	if !h.daemonAlone() {
		t.Fatal("the daemon and what it hosts")
	}
	for _, intruder := range []func(){
		func() { alive(t, h.home, "sessions/3.json", os.Getpid()) },
		func() { writeFile(t, filepath.Join(h.home, "sessions", "3.json"), `{"pid":`) },
		func() { h.others = true },
	} {
		intruder()
		if h.daemonAlone() {
			t.Fatal("something that outlives the daemon is there")
		}
		os.Remove(filepath.Join(h.home, "sessions", "3.json"))
		h.others = false
	}
	os.Remove(filepath.Join(h.home, "daemon.lock"))
	if h.daemonAlone() {
		t.Fatal("no daemon, nothing to stop")
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
	at := later(3 * time.Hour)
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-rot", "R-rot", at, "1999999999000"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-rot" || a.Token.Access != "A-rot" || a.Token.ExpiresAt != at {
		t.Fatalf("the rotation is the account's now: %+v", a.Token)
	}
	if n := f.profiles.Load() + f.refreshes.Load() + f.usages.Load(); n != 0 {
		t.Fatalf("a plain rotation asks nobody anything: %d calls", n)
	}
}

// A login somebody made with /login inside the home is a new login, and it
// is checked with the provider before anything of it is taken: the same
// account's is taken — a dead account comes back with it — and somebody
// else's is refused with the account's own tokens left exactly as they were.
func TestANewLoginInTheHomeIsCheckedBeforeItIsTaken(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	newLogin := storeLogin("A-login", "R-login", later(5*time.Hour), "1777777777000")

	t.Run("this account's", func(t *testing.T) {
		f.set(func(f *anthropic) { f.profile = profileOf("A-login", "u1") })
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.Dead, a.DeadReason = true, "invalid_grant"
		a.Staged = ""
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), newLogin)
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		if a.Dead || a.Token.Refresh != "R-login" || a.Extra["refresh_token_expires_at"] != "1777777777000" {
			t.Fatalf("somebody signed it in again inside Claude Code: %+v", a)
		}
	})
	t.Run("somebody else's", func(t *testing.T) {
		f.set(func(f *anthropic) { f.profile = profileOf("A-login", "u9") })
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), newLogin)
		before := a.Token
		if err := s.claudeHome(a, false).adopt(context.Background()); err != nil {
			t.Fatal(err)
		}
		if a.Token.Refresh != before.Refresh || a.Token.Access != before.Access || a.Staged != "-" {
			t.Fatalf("not taken, and the home is marked for the account's own login: %+v %q", a.Token, a.Staged)
		}
		if len(*words) != 1 || !strings.Contains((*words)[0], "u9@x") || !strings.Contains((*words)[0], "u1@x") {
			t.Fatalf("said, naming both: %q", *words)
		}
		// The next quiet launch puts the account's own login back.
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
			t.Fatalf("its own login goes back in: %q", r)
		}
	})
}

// The morning after `rota login`: the seed carried no refreshTokenExpiresAt,
// Claude Code added one on its first refresh — which reads as a new login —
// and that login's access token has lapsed since, so the provider will not
// say whose it is. rota must not then present the account's own refresh
// token, which that login may have replaced: nothing refreshes it — not a
// launch, not a listing, not maintenance — nothing is written, the account
// does not die, and the launch runs on the home's login as it is. Once the
// login can be confirmed it is taken.
func TestALoginMadeInsideClaudeCodeThatCannotBeConfirmedIsUsedAndNeverRefreshedOver(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.profile = func(string) (int, any) { return 401, nil }
		f.refresh = refreshesTo("A-spent", "R-spent")
	})
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	delete(a.Extra, "refresh_token_expires_at") // what `rota login` leaves
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	cc := storeLogin("A-cc", "R-cc", later(-time.Hour), "1999999999000")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), cc)
	expire(a)
	same := func(when string) {
		t.Helper()
		if f.refreshes.Load() != 0 || a.Dead || a.Token.Refresh != "R-u1" || readFile(filepath.Join(s.Home(a), ".credentials.json")) != cc {
			t.Fatalf("%s: refreshes=%d dead=%v account=%q home changed=%v", when, f.refreshes.Load(), a.Dead, a.Token.Refresh,
				readFile(filepath.Join(s.Home(a), ".credentials.json")) != cc)
		}
	}

	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatal(err)
	}
	if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || !slices.Equal(configDirs(env), []string{s.Home(a)}) {
		t.Fatalf("on the stored route, joining the home's login: %v", env)
	}
	same("launch")
	if len(*words) != 1 || !strings.Contains((*words)[0], "not been able to confirm") {
		t.Fatalf("one line says so: %q", *words)
	}
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	same("listing")
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	same("maintenance")
	if _, _, err := s.prepare(context.Background(), a, false); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a run without the home and on an expired token is refused: %v", err)
	}
	same("hermetic run")

	// Claude Code, started there, refreshed it, and now it can be confirmed.
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-cc2", "R-cc2", later(time.Hour), "1999999999000"))
	f.set(func(f *anthropic) { f.profile = profileOf("A-cc2", "u1") })
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if a.Token.Refresh != "R-cc2" || a.Dead || f.refreshes.Load() != 0 {
		t.Fatalf("taken once confirmed: %+v", a.Token)
	}
}

// A dead account whose home holds a login nobody could confirm yet still
// launches, on that login: Claude Code starting there is what makes it
// confirmable again.
func TestADeadAccountWithAnUnconfirmedLoginInItsHomeLaunches(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.profile = func(string) (int, any) { return 503, nil } })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.Dead, a.DeadReason = true, "invalid_grant"
	login := storeLogin("A-login", "R-login", later(time.Hour), "1777777777000")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), login)
	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatalf("let through: %v", err)
	}
	if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || readFile(filepath.Join(s.Home(a), ".credentials.json")) != login {
		t.Fatalf("on the home's login, nothing written: %v", env)
	}
	if f.refreshes.Load() != 0 {
		t.Fatal("nothing refreshed")
	}
	// Without a usable login in its home, a dead account is refused as ever.
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`)
	if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrReauth) {
		t.Fatalf("refused: %v", err)
	}
}

// When the provider refuses rota's own refresh as dead, the home is read
// again before that is believed. A sibling's login there that can be
// confirmed is taken; one that cannot means the account's copy was stale,
// not the login dead: the account stays alive, in the hold, and the launch
// runs on the home's login.
func TestARefusedRefreshIsNotBelievedUntilTheHomeIsReadAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	for _, c := range []struct {
		what      string
		profile   func(string) (int, any)
		refresh   string // the account's refresh token afterwards
		storedEnv bool
	}{
		{"a sibling's login that can be confirmed", profileOf("A-sib", "u1"), "R-sib", true},
		{"one that cannot", func(string) (int, any) { return 401, nil }, "R-u1", true},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := openTemp(t)
			a := livingClaude(s, "u1")
			expire(a)
			home := s.Home(a)
			f.set(func(f *anthropic) {
				f.profile = c.profile
				f.refresh = func() (int, any) {
					// The sibling got there first: the home holds its login now.
					writeFile(t, filepath.Join(home, ".credentials.json"),
						storeLogin("A-sib", "R-sib", later(time.Hour), "1888888888000"))
					return 400, map[string]string{"error": "invalid_grant"}
				}
			})
			env, err := launchEnv(t, s, a)
			if err != nil {
				t.Fatalf("the login is alive: %v", err)
			}
			if a.Dead || a.Token.Refresh != c.refresh {
				t.Fatalf("dead=%v refresh=%q", a.Dead, a.Token.Refresh)
			}
			if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(readFile(filepath.Join(home, ".credentials.json")), "R-sib") {
				t.Fatalf("the launch runs on the home's login, which is left as it is: %v", env)
			}
		})
	}
}

// A store that is there and cannot be read is not an empty one. While
// anything runs in the home it is held: nothing refreshed or written, a run
// on a token only with one that is good. In a quiet home it is read once more
// after a pause and only then taken for empty and written over. A keychain
// that cannot be read is held too, with one line, and fails nothing.
func TestAnUnreadableStoreIsNotAnEmptyOne(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	old := unreadablePause
	unreadablePause = time.Millisecond
	t.Cleanup(func() { unreadablePause = old })
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	torn := `{"claudeAiOauth":{"refreshToken":"R-cc"`

	t.Run("while something runs", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), torn)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		env, err := launchEnv(t, s, a)
		if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
			t.Fatalf("a good token, in the environment: %v %v", err, env)
		}
		expire(a)
		if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrBusy) {
			t.Fatalf("an expired one nothing may refresh is refused: %v", err)
		}
		if f.refreshes.Load() != 0 || readFile(filepath.Join(s.Home(a), ".credentials.json")) != torn {
			t.Fatal("nothing refreshed, nothing written")
		}
	})
	t.Run("in a quiet home", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), torn)
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
			t.Fatalf("taken for empty after a second look, and written: %q", r)
		}
	})
	t.Run("a keychain that cannot be read", func(t *testing.T) {
		k := fakeKeychain(t)
		k.findCode = 36
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		env, err := launchEnv(t, s, a)
		if err != nil || !hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("not a failed launch: %v %v", err, env)
		}
		if len(*words) != 1 || !strings.Contains((*words)[0], "keychain") {
			t.Fatalf("one line: %q", *words)
		}
		if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
			t.Fatal("nothing written")
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
	at := later(4 * time.Hour)
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-file", "R-file", at, "1999999999000"))
	k.items[service(t, s.Home(a))] = storeLogin("A-key", "R-key", at, "1999999999000")
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
	a.Token.ExpiresAt = later(2 * time.Hour)
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-newer" {
		t.Fatalf("written now: %q", r)
	}
}

// A home holding an older login of the account's — one that expires before
// the account's own token — is behind, not in sync: a quiet launch writes the
// current login over it, and a live one joins it and says when the current
// one takes over. Left alone, Claude Code would start on the old token, be
// refused, and sign the account out.
func TestAHomeHoldingAnOlderLoginIsWrittenWhenQuietAndJoinedWhenNot(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	older := storeLogin("A-old", "R-old", a.Token.ExpiresAt-60_000, "1999999999000")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), older)
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("joined, on the stored route: %v %v", err, env)
	}
	if readFile(filepath.Join(s.Home(a), ".credentials.json")) != older || len(*words) != 1 || !strings.Contains((*words)[0], "/login") {
		t.Fatalf("nothing written while it runs, and said: %q", *words)
	}
	os.Remove(filepath.Join(s.Home(a), "daemon.lock"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
		t.Fatalf("quiet: the current login goes in: %q", r)
	}
}

// With Claude Code alive in the home and no usable login there — windows
// from before this rota, on tokens in their environment — this launch goes
// the same way rather than start a Claude Code that is not signed in. The
// account's refresh token is then in rota's store alone, so an expired one
// is refreshed, as it always was. Nothing is written, and the daemon those
// windows started is left alone: stopping it would not make the home quiet.
func TestAnAliveHomeWithNoLoginLaunchesOnATokenAndNeverLoggedOut(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	d := fakeDaemons(t)
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("on a token: %v %v", err, env)
	}
	if len(*words) == 0 || !strings.Contains((*words)[len(*words)-1], "without Remote Control") {
		t.Fatalf("said: %q", *words)
	}
	expire(a)
	env, err = launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-new") || f.refreshes.Load() != 1 {
		t.Fatalf("refreshed, and on the new token: %v %v", err, env)
	}
	if _, err := os.Stat(filepath.Join(s.Home(a), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("and nothing written")
	}
	if d.stops() != 0 {
		t.Fatal("the daemon is left alone")
	}
}

/* --------------------------------------------------------------- macOS --- */

// On macOS the new file is written and synced first, the keychain item is
// deleted next, and only then is the file renamed into place; nothing is
// ever written into the keychain. A delete that fails leaves everything as
// it was.
func TestSeedingOnMacOSWritesTheFileBeforeDeletingTheItemAndRenamesAfter(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	svc := service(t, home)
	inItem := `{"mcpOAuth":{"from":"keychain"},"claudeAiOauth":{"refreshToken":"R-old","expiresAt":1,"refreshTokenExpiresAt":1999999999000}}`
	k.items[svc] = inItem
	var tmpAtDelete, fileAtDelete bool
	k.onDelete = func(string) {
		tmps, _ := filepath.Glob(filepath.Join(home, ".credentials.json-*.tmp"))
		tmpAtDelete = len(tmps) == 1
		_, err := os.Stat(filepath.Join(home, ".credentials.json"))
		fileAtDelete = err == nil
	}
	k.deleteCode = 36
	if _, err := launchEnv(t, s, a); err == nil {
		t.Fatal("a delete that fails stops the write")
	}
	if v, ok := k.item(svc); !ok || v != inItem {
		t.Fatal("the item is as it was")
	}
	if tmps, _ := filepath.Glob(filepath.Join(home, "*.tmp")); len(tmps) != 0 {
		t.Fatalf("the temporary file goes: %v", tmps)
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("and no file appears")
	}

	k.deleteCode = 0
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if !tmpAtDelete || fileAtDelete {
		t.Fatalf("written and synced before the delete, renamed after: tmp=%v file=%v", tmpAtDelete, fileAtDelete)
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("the item is gone")
	}
	r, doc := readLogin(t, home)
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

// A home whose path is not plain ASCII cannot take the stored route on macOS
// at all: the keychain item Claude Code would keep its login in cannot be
// named, so rota could not follow it. It runs on a token, says so once, asks
// nothing of the keychain, writes nothing, and has no Remote Control.
func TestAHomeWithANonASCIIPathRunsOnATokenOnMacOS(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	a.ConfigDir = filepath.Join(t.TempDir(), "projét")
	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("on a token: %v %v", err, env)
	}
	if got := k.asked(); len(got) != 0 {
		t.Fatalf("nothing asked of the keychain: %v", got)
	}
	if _, err := os.Stat(filepath.Join(a.ConfigDir, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("nothing written")
	}
	if len(*words) != 1 || !strings.Contains((*words)[0], "ASCII") || !strings.Contains((*words)[0], "Remote Control") {
		t.Fatalf("said once, with why: %q", *words)
	}
	a.RemoteControl = true
	if err := s.RemoteControlNow(a); !errors.Is(err, rota.ErrUnsupported) {
		t.Fatalf("no Remote Control there: %v", err)
	}
}

/* ---------------------------------------------------------- refreshing --- */

// rota refreshes a login kept in the home only while nothing runs there,
// saves it, and only then writes it into the home.
func TestMaintenanceRefreshesOnlyAQuietHomeAndSavesBeforeItWrites(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	expire(a)
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	s.Maintain(context.Background())
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatal("not while Claude Code holds the login")
	}
	os.Remove(filepath.Join(s.Home(a), "sessions", "1.json"))
	ob.watch(s, a, "R-new")
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 || a.Token.Refresh != "R-new" {
		t.Fatalf("refreshed: %d %+v", f.refreshes.Load(), a.Token)
	}
	ob.savedThenWritten(t)
}

// A listing refreshes an expired login of a quiet home in the same order.
func TestAListingSavesARefreshedLoginBeforeItWritesIt(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	expire(a)
	ob.watch(s, a, "R-new")
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	ob.savedThenWritten(t)
	if s.claimed(a) {
		t.Fatal("the claim the refresh took is let go")
	}
}

// Usage is read with the token each account has, while Claude Code runs or
// not, for several accounts at once. A 401 is Claude Code having rotated the
// login meanwhile: the home is read again and the reading tried once more.
func TestUsageIsReadWhileAliveAndRetriedOnceAfterA401(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a, b := livingClaude(s, "u1"), livingClaude(s, "u2")
	homes := map[string]string{"A-u1": s.Home(a), "A-u2": s.Home(b)}
	for _, x := range []*rota.Account{a, b} {
		if _, err := launchEnv(t, s, x); err != nil {
			t.Fatal(err)
		}
		alive(t, s.Home(x), "daemon.lock", os.Getpid())
	}
	f.set(func(f *anthropic) {
		f.usage = func(auth string) (int, any) {
			old := strings.TrimPrefix(auth, "Bearer ")
			if home, ok := homes[old]; ok {
				// Claude Code rotates between the reading of the home and the
				// request.
				u := strings.TrimPrefix(old, "A-")
				writeFile(t, filepath.Join(home, ".credentials.json"),
					storeLogin("A-rot-"+u, "R-rot-"+u, later(2*time.Hour), "1999999999000"))
				return 401, nil
			}
			return 200, map[string]any{"five_hour": map[string]any{"utilization": 42}}
		}
	})
	if errs := s.Refresh(context.Background(), true, a, b); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, x := range []*rota.Account{a, b} {
		if x.Quota == nil || x.Quota.Windows[0].Percent != 42 || !strings.HasPrefix(x.Token.Access, "A-rot-") {
			t.Fatalf("%s: read with the rotated token: %+v %+v", x, x.Quota, x.Token)
		}
	}
	if f.usages.Load() != 4 || f.refreshes.Load() != 0 {
		t.Fatalf("once with each old token, once with each rotated one, nothing refreshed: %d %d", f.usages.Load(), f.refreshes.Load())
	}
}

// Many claude accounts in one listing, each home rotated meanwhile: every
// rotation is taken, and the work runs side by side without one account's
// adoption racing another's reading.
func TestAListingOfManyClaudeAccountsTakesEveryRotation(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	fakeAnthropic(t)
	s := openTemp(t)
	var accounts []*rota.Account
	for _, u := range []string{"u1", "u2", "u3", "u4"} {
		x := livingClaude(s, u)
		if _, err := launchEnv(t, s, x); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(s.Home(x), ".credentials.json"),
			storeLogin("A-rot-"+u, "R-rot-"+u, later(3*time.Hour), "1999999999000"))
		accounts = append(accounts, x)
	}
	if errs := s.Refresh(context.Background(), true, accounts...); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, x := range accounts {
		if !strings.HasPrefix(x.Token.Refresh, "R-rot-") || x.Quota == nil {
			t.Fatalf("%s: %+v", x, x.Token)
		}
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

// A store whose lock was released — a run started since it was opened, as
// `--with quota` reads after one — rotates nothing: no refresh request, no
// claim, nothing written, and the refresh token on disk is the one it was.
func TestAStoreWhoseLockWasReleasedRotatesNothing(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.Long = &rota.LongToken{Access: "LONG", ExpiresAt: later(300 * 24 * time.Hour)}
	expire(a)
	_, release, err := s.ready(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatalf("nothing fails either: %v", errs)
	}
	s.Maintain(context.Background())
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatalf("nothing rotated: %d %q", f.refreshes.Load(), a.Token.Refresh)
	}
	id, dir := a.ID, storeDir(s)
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Find(id).Token.Refresh; got != "R-u1" {
		t.Fatalf("on disk: %q", got)
	}
	if _, err := os.Stat(filepath.Join(s2.Home(s2.Find(id)), ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("nothing written into the home")
	}
}

/* --------------------------------------------- the daemon, and removing --- */

// The daemon is stopped only for a change of route the launch really makes,
// and only when stopping it leaves the home quiet. A daemon started on a
// token, alone with what it hosts, is stopped by the launch that moves the
// account onto its stored login; one beside a window somebody has open is
// left, and the launch goes on a token beside it, as many times as it is
// asked; a launch back onto a token stops one started on the stored login;
// and one that will not stop is said and fails nothing.
func TestTheDaemonIsStoppedOnlyForARouteChangeThatLeavesTheHomeQuiet(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	d := fakeDaemons(t)
	d.then = stopsClean
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "from-the-shell")

	t.Run("a token daemon alone is stopped for the stored route", func(t *testing.T) {
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		alive(t, s.Home(a), "daemon.lock", os.Getpid())
		hosted(t, s.Home(a), "sessions/1.json", os.Getpid())
		env, err := launchEnv(t, s, a)
		if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("the stored route: %v %v", err, env)
		}
		if d.stops() != 1 || d.homes[0] != s.Home(a) {
			t.Fatalf("stopped once, in the account's home: %v", d.homes)
		}
		if !slices.Contains(d.envs[0], "CLAUDE_CONFIG_DIR="+s.Home(a)) || hasVar(d.envs[0], "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("pointed at the home, and not authenticated: %v", d.envs[0])
		}
		if len(*words) == 0 || !strings.Contains((*words)[0], "stopped the Claude Code daemon") {
			t.Fatalf("said: %q", *words)
		}
		if r, _ := readLogin(t, s.Home(a)); r != "R-u1" {
			t.Fatal("with the daemon gone the home is quiet, and the login goes in")
		}
		alive(t, s.Home(a), "daemon.lock", os.Getpid())
		if _, err := launchEnv(t, s, a); err != nil || d.stops() != 1 {
			t.Fatalf("a daemon on the same route is left alone: %v %d", err, d.stops())
		}
	})
	t.Run("beside an open window it is left, every launch", func(t *testing.T) {
		before := d.stops()
		s := openTemp(t)
		a := livingClaude(s, "u1")
		alive(t, s.Home(a), "daemon.lock", os.Getpid())
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		for i := 0; i < 3; i++ {
			env, err := launchEnv(t, s, a)
			if err != nil || !hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || a.Extra[routeKey] != routeEnv {
				t.Fatalf("launch %d on a token: %v %v", i, err, env)
			}
		}
		if d.stops() != before {
			t.Fatal("nothing stopped")
		}
	})
	t.Run("back onto a token stops a stored-login daemon", func(t *testing.T) {
		before := d.stops()
		s := openTemp(t)
		a := livingClaude(s, "u1")
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		alive(t, s.Home(a), "daemon.lock", os.Getpid())
		a.Dead, a.DeadReason = true, "invalid_grant"
		a.Long = &rota.LongToken{Access: "LONG", ExpiresAt: later(300 * 24 * time.Hour)}
		env, err := launchEnv(t, s, a)
		if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=LONG") || d.stops() != before+1 {
			t.Fatalf("on the long token, the stored daemon stopped: %v %v %d", err, env, d.stops()-before)
		}
	})
	t.Run("one that will not stop", func(t *testing.T) {
		d.err, d.then = errors.New("no"), nil
		defer func() { d.err, d.then = nil, stopsClean }()
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		alive(t, s.Home(a), "daemon.lock", os.Getpid())
		env, err := launchEnv(t, s, a)
		if err != nil || !hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("fails nothing, and goes on a token beside it: %v %v", err, env)
		}
		if len(*words) == 0 || !strings.Contains((*words)[0], "could not be stopped") {
			t.Fatalf("said: %q", *words)
		}
	})
}

// In a directory the person chose, nothing remembered about the route is
// nothing known: a daemon running there may be anybody's, and is left alone.
func TestADaemonInAChosenDirectoryWithNoRouteRememberedIsLeftAlone(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	d := fakeDaemons(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.ConfigDir = t.TempDir()
	alive(t, a.ConfigDir, "daemon.lock", os.Getpid())
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if d.stops() != 0 {
		t.Fatal("nothing stopped")
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
	d.then = stopsClean
	homeAtDelete := false
	k.onDelete = func(string) {
		_, err := os.Stat(home)
		homeAtDelete = err == nil
	}
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if d.stops() != 1 || !homeAtDelete {
		t.Fatalf("daemon stopped %v; the item went while the home was still there: %v", d.homes, homeAtDelete)
	}
	if _, ok := k.item(svc); ok {
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

/* ----------------------------------------------------- whose home is it --- */

// A rota started inside a session it launched inherits CLAUDE_CONFIG_DIR
// naming that account's home, and an account's home is never the person's:
// a listing reads it and refreshes nothing while it runs; a launch of the
// same account stays on its stored login, stops nothing, and mirrors the
// person's own directory rather than the home into itself; and removing the
// account takes its keychain item and its login.
func TestARotaStartedInsideAnAccountsSessionKnowsTheHomeIsTheAccounts(t *testing.T) {
	storedRouteHere(t)
	person := t.TempDir()
	t.Setenv("HOME", person)
	src := filepath.Join(person, ".claude")
	writeFile(t, filepath.Join(src, "settings.json"), "{}")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	k := fakeKeychain(t)
	d := fakeDaemons(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	home := s.Home(a)
	t.Setenv("CLAUDE_CONFIG_DIR", home) // what the Claude Code rota launched hands its children
	alive(t, home, "sessions/1.json", os.Getpid())
	alive(t, home, "daemon.lock", os.Getpid())

	expire(a)
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatal("a listing refreshes nothing while Claude Code runs there")
	}
	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || !slices.Equal(configDirs(env), []string{home}) {
		t.Fatalf("the same account, on its stored login: %v %v", err, env)
	}
	if d.stops() != 0 {
		t.Fatal("nothing stopped")
	}
	linksTo(t, filepath.Join(home, "settings.json"), filepath.Join(src, "settings.json"))

	svc := service(t, home)
	k.items[svc] = "{}"
	os.Remove(filepath.Join(home, "sessions", "1.json"))
	os.Remove(filepath.Join(home, "daemon.lock"))
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("its keychain item goes")
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("and its login")
	}
}

// One home holds one account's login. Two accounts cannot be told the same
// directory, and two that already were are not launched there, nor do they
// read each other's login as their own.
func TestTwoAccountsCannotShareAHome(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a, b := livingClaude(s, "uA"), livingClaude(s, "uB")
	dir := t.TempDir()
	a.ConfigDir = dir
	want := *b
	want.ConfigDir = dir
	if err := s.CheckHome(&want); err == nil || !strings.Contains(err.Error(), a.String()) {
		t.Fatalf("refused, naming the account it belongs to: %v", err)
	}
	if err := s.CheckHome(&rota.Account{ID: 99, Provider: "codex", ConfigDir: dir}); err == nil {
		t.Fatal("whatever the provider")
	}
	b.ConfigDir = dir // as a store written before the rule might hold it
	writeFile(t, filepath.Join(dir, ".credentials.json"), storeLogin("A-uB", "R-uB", later(2*time.Hour), "1999999999000"))
	_, err := launchEnv(t, s, a)
	if err == nil || !strings.Contains(err.Error(), a.String()) || !strings.Contains(err.Error(), b.String()) {
		t.Fatalf("refused, naming both: %v", err)
	}
	if a.Token.Refresh != "R-uA" {
		t.Fatalf("and B's login is not taken: %q", a.Token.Refresh)
	}
	if r, _ := readLogin(t, dir); r != "R-uB" {
		t.Fatal("nothing written")
	}
}

// A claude account given another home leaves no copy of its login behind:
// not while anything runs in the old one, and when nothing does, the login
// is taken out of it — keychain item first — and the new home is written
// from nothing, its daemon's route its own.
func TestMovingAClaudeAccountsHomeTakesItsLoginWithIt(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	old := s.Home(a)
	svc := service(t, old)
	k.items[svc] = "{}"
	next := t.TempDir()
	alive(t, old, "daemon.lock", os.Getpid())
	if err := s.MoveHome(a, next); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("not while it runs there: %v", err)
	}
	if r, _ := readLogin(t, old); r != "R-u1" {
		t.Fatal("and nothing changed")
	}
	os.Remove(filepath.Join(old, "daemon.lock"))
	fileAtDelete := false
	k.onDelete = func(string) {
		_, err := os.Stat(filepath.Join(old, ".credentials.json"))
		fileAtDelete = err == nil
	}
	if err := s.MoveHome(a, next); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.item(svc); ok || !fileAtDelete {
		t.Fatal("the keychain item goes first")
	}
	if _, err := os.Stat(filepath.Join(old, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatal("then the file")
	}
	if a.Staged != "-" || a.Extra[routeKey] != "" {
		t.Fatalf("and the account forgets the home: %q %q", a.Staged, a.Extra[routeKey])
	}
	a.ConfigDir = next
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, next); r != "R-u1" {
		t.Fatalf("the new home is written: %q", r)
	}
	if err := s.MoveHome(a, next); err != nil {
		t.Fatal("staying where it is is nothing")
	}
}

// The homes are under an absolute directory, whatever the store was opened
// with: Claude Code is pointed at a home's path, and its keychain item is
// named for it, and neither may depend on where rota was started.
func TestAccountHomesAreAbsoluteWhateverTheStoreWasOpenedWith(t *testing.T) {
	t.Chdir(t.TempDir())
	s, err := Open("relative-store")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := livingClaude(s, "u1")
	if !filepath.IsAbs(s.Home(a)) || !filepath.IsAbs(s.Backend().HomeRoot()) {
		t.Fatalf("%s %s", s.Home(a), s.Backend().HomeRoot())
	}
	if runtime.GOOS != "windows" {
		if svc, _ := os.Getwd(); !strings.HasPrefix(s.Home(a), svc) && !strings.Contains(s.Home(a), "relative-store") {
			t.Fatalf("under where it was opened: %s", s.Home(a))
		}
	}
}
