package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

/* -------------------------------------------- a refused token, never again --- */

// The morning after `rota login`, its Staged "-": the account's own token is
// refused once, and the home holds a login made inside Claude Code that
// nobody can confirm. From then on the refused token is never presented
// again — not by three listings, a launch, a hermetic run or maintenance —
// the account is dead and says what is waiting, the launch in its home still
// starts on the home's login, and once that login can be confirmed the
// account is alive again.
func TestARefusedRefreshTokenIsNeverPresentedAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.profile = func(string) (int, any) { return 401, nil }
		f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
	})
	s := openTemp(t)
	a := livingClaude(s, "u1") // Staged "-", as `rota login` leaves it
	home := s.Home(a)
	login := storeLogin("A-login", "R-login", later(-time.Hour), "1777777777000")
	writeFile(t, filepath.Join(home, ".credentials.json"), login)
	expire(a)

	for i := 0; i < 3; i++ {
		s.Refresh(context.Background(), true, a)
	}
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("one refusal, and the token is never presented again: %d", n)
	}
	if !a.Dead || !strings.Contains(a.DeadReason, "waiting to be confirmed") {
		t.Fatalf("dead, and saying why: %v %q", a.Dead, a.DeadReason)
	}
	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || !slices.Equal(configDirs(env), []string{home}) {
		t.Fatalf("the launch in its home starts, on the home's login: %v %v", err, env)
	}
	if _, _, err := s.prepare(context.Background(), a, false); err == nil {
		t.Fatal("a hermetic run on an expired token is refused")
	}
	s.Maintain(context.Background())
	if n := f.refreshes.Load(); n != 1 {
		t.Fatalf("nothing presented it again: %d", n)
	}
	if readFile(filepath.Join(home, ".credentials.json")) != login {
		t.Fatal("and the home's login is as it was")
	}
	// Claude Code refreshed the home's login, and now it can be confirmed.
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-cc", "R-cc", later(time.Hour), "1777777777000"))
	f.set(func(f *anthropic) { f.profile = profileOf("A-cc", "u1") })
	s.Refresh(context.Background(), true, a)
	if a.Dead || a.Token.Refresh != "R-cc" || f.refreshes.Load() != 1 {
		t.Fatalf("alive again, on the confirmed login: dead=%v %q", a.Dead, a.Token.Refresh)
	}
}

/* ----------------------------- refreshing beside what runs in the home --- */

// A home that was on the account's stored login and shows no login now while
// Claude Code runs there — a sibling window ran /logout, the store was caught
// mid-replace — is not a home of old windows on tokens: a process there may
// hold the account's refresh token in memory. Nothing refreshes it: the
// launch runs only on a token that is good, a hermetic run likewise, and both
// are refused otherwise.
func TestAHomeOnceOnTheStoredRouteShowingNoLoginWhileAliveIsHeld(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	home := s.Home(a)
	alive(t, home, "sessions/1.json", os.Getpid())
	os.Remove(filepath.Join(home, ".credentials.json"))

	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("a token that is good still runs: %v %v", err, env)
	}
	if len(*words) == 0 || !strings.Contains((*words)[0], "shows none now while Claude Code runs there") {
		t.Fatalf("said: %q", *words)
	}
	expire(a)
	if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("an expired one is refused: %v", err)
	}
	if _, _, err := s.prepare(context.Background(), a, false); !errors.Is(err, rota.ErrBusy) {
		t.Fatalf("a hermetic run too: %v", err)
	}
	if f.refreshes.Load() != 0 || a.Dead {
		t.Fatal("and nothing refreshed")
	}
}

/* ------------------------------------------------- one directory, two names --- */

// On a volume that ignores case, …/Shared and …/shared are one directory.
// It is one home: a second account cannot be told it, two that were are not
// launched there, and ~/.Claude is the person's own as ~/.claude is.
func TestOneDirectoryUnderTwoSpellingsIsOneHome(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	dir := filepath.Join(t.TempDir(), "Shared")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(dir), "shared")
	if !sameDir(dir, other) {
		t.Skip("this file system tells the two spellings apart")
	}
	s := openTemp(t)
	a, b := livingClaude(s, "uA"), livingClaude(s, "uB")
	a.ConfigDir = dir
	want := *b
	want.ConfigDir = other
	if err := s.CheckHome(&want); err == nil {
		t.Fatal("a second spelling is the same home")
	}
	b.ConfigDir = other
	if _, err := launchEnv(t, s, b); err == nil || !strings.Contains(err.Error(), a.String()) {
		t.Fatalf("not launched there: %v", err)
	}
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := livingClaude(s, "uC")
	c.ConfigDir = filepath.Join(home, ".Claude")
	if !s.personalClaude(c) || s.loginInHome(c) {
		t.Fatal("~/.Claude is the person's own")
	}
	if err := s.CheckHome(&rota.Account{ID: 99, Provider: "claude", ConfigDir: strings.ToUpper(s.homeRoot[:1]) + s.homeRoot[1:] + "/claude-1"}); err == nil {
		t.Fatal("nor does a second spelling of rota's own homes get past the rule")
	}
}

/* ----------------------------------------------- whose directory it is --- */

// Whose Claude Code directory is the person's is told down by the rota that
// launched this one, never guessed from what it inherited: ROTA_CLAUDE_HOME
// names it, or is empty for ~/.claude; launched by a rota that did not say,
// an inherited CLAUDE_CONFIG_DIR is somebody's home or a hermetic run's
// throwaway directory, and the default is used; and only a rota nobody
// launched takes CLAUDE_CONFIG_DIR for the person's own. Every claude launch
// says it on, on both routes.
func TestWhoseDirectoryIsThePersonsIsToldDownNotGuessed(t *testing.T) {
	storedRouteHere(t)
	person := t.TempDir()
	t.Setenv("HOME", person)
	custom := filepath.Join(t.TempDir(), "my-claude")
	writeFile(t, filepath.Join(custom, "settings.json"), "{}")
	writeFile(t, filepath.Join(person, ".claude", "settings.json"), "{}")
	throwaway := filepath.Join(t.TempDir(), "rota-hermetic-1")
	writeFile(t, filepath.Join(throwaway, "settings.json"), "{}")

	for _, c := range []struct {
		what              string
		claudeHome        *string
		accountID, cfgDir string
		want, handedDown  string
	}{
		{"nobody launched it", nil, "", custom, custom, custom},
		{"told a custom directory", ptr(custom), "7", throwaway, custom, custom},
		{"told the default", ptr(""), "7", throwaway, filepath.Join(person, ".claude"), ""},
		{"launched by a rota that did not say", nil, "7", throwaway, filepath.Join(person, ".claude"), ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			if c.claudeHome == nil {
				os.Unsetenv(claudeHomeVar)
			} else {
				t.Setenv(claudeHomeVar, *c.claudeHome)
			}
			t.Setenv("ROTA_ACCOUNT_ID", c.accountID)
			if c.accountID == "" {
				os.Unsetenv("ROTA_ACCOUNT_ID")
			}
			t.Setenv("CLAUDE_CONFIG_DIR", c.cfgDir)
			if got := PersonalClaudeDir(); got != c.want {
				t.Fatalf("the person's directory: %q, want %q", got, c.want)
			}
			s := openTemp(t)
			a := livingClaude(s, "u1")
			env, err := launchEnv(t, s, a)
			if err != nil {
				t.Fatal(err)
			}
			linksTo(t, filepath.Join(s.Home(a), "settings.json"), filepath.Join(c.want, "settings.json"))
			if !slices.Contains(env, claudeHomeVar+"="+c.handedDown) {
				t.Fatalf("told down: %v", env)
			}
			cmd, release, err := s.prepare(context.Background(), a, false)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if !slices.Contains(cmd.Env, claudeHomeVar+"="+c.handedDown) {
				t.Fatalf("on the environment route too: %v", cmd.Env)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// The person's own directory is theirs whatever an account is told: a
// custom CLAUDE_CONFIG_DIR of theirs named for an account keeps that account
// on a token, and neither the login in that directory nor its keychain item
// is read, written or removed.
func TestTheLoginInThePersonsOwnDirectoryIsNeverTouched(t *testing.T) {
	storedRouteHere(t)
	x := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", x)
	writeFile(t, filepath.Join(x, ".credentials.json"), storeLogin("A-person", "R-person", later(5*time.Hour), "1888888888000"))
	k := fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	a.ConfigDir = x
	svc := service(t, x)
	k.items[svc] = storeLogin("A-person", "R-person", later(5*time.Hour), "1888888888000")
	env, err := launchEnv(t, s, a)
	if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
		t.Fatalf("on a token: %v %v", err, env)
	}
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.item(svc); !ok || readLogin2(t, x) != "R-person" || len(k.asked()) != 0 {
		t.Fatalf("the person's login is never touched: %v", k.asked())
	}
}

// In a directory the person chose — not known to be theirs — a login rota
// did not write is confirmed first, whatever is recorded. Somebody else's is
// left exactly where it is, keychain item and file, and the account runs on
// its own token beside it, said once; moving or removing the account leaves
// it too. The account's own is taken, at the cost of one reading of the
// profile. One nobody can confirm is the hold.
func TestAChosenDirectorysLoginIsNeverReplacedUnlessItIsTheAccounts(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	k := fakeKeychain(t)
	somebody := storeLogin("A-sb", "R-sb", later(5*time.Hour), "1888888888000")

	t.Run("somebody else's", func(t *testing.T) {
		f.set(func(f *anthropic) { f.profile = profileOf("A-sb", "u9") })
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		a.ConfigDir = t.TempDir()
		a.StagedWritten() // whatever is recorded, it is not this login
		svc := service(t, a.ConfigDir)
		k.items[svc] = somebody
		writeFile(t, filepath.Join(a.ConfigDir, ".credentials.json"), somebody)
		env, err := launchEnv(t, s, a)
		if err != nil || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=A-u1") {
			t.Fatalf("on its own token: %v %v", err, env)
		}
		if len(*words) != 1 || !strings.Contains((*words)[0], "another account's login") || !strings.Contains((*words)[0], "u9@x") {
			t.Fatalf("said once: %q", *words)
		}
		if v, _ := k.item(svc); v != somebody || readFile(filepath.Join(a.ConfigDir, ".credentials.json")) != somebody {
			t.Fatal("left exactly where it is")
		}
		next := t.TempDir()
		if err := s.MoveHome(context.Background(), a, next); err != nil {
			t.Fatal(err)
		}
		if v, _ := k.item(svc); v != somebody {
			t.Fatal("a move leaves it")
		}
	})
	t.Run("the account's own, rotated", func(t *testing.T) {
		f.profiles.Store(0)
		f.set(func(f *anthropic) { f.profile = profileOf("A-rot", "u1") })
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.ConfigDir = t.TempDir()
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		k.items[service(t, a.ConfigDir)] = storeLogin("A-rot", "R-rot", later(5*time.Hour), "1999999999000")
		if _, err := launchEnv(t, s, a); err != nil {
			t.Fatal(err)
		}
		if a.Token.Refresh != "R-rot" || f.profiles.Load() != 1 {
			t.Fatalf("taken, once confirmed: %q %d", a.Token.Refresh, f.profiles.Load())
		}
	})
	t.Run("one nobody can confirm", func(t *testing.T) {
		f.set(func(f *anthropic) { f.profile = func(string) (int, any) { return 503, nil } })
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.ConfigDir = t.TempDir()
		k.items[service(t, a.ConfigDir)] = somebody
		env, err := launchEnv(t, s, a)
		if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("the hold joins it: %v %v", err, env)
		}
		if v, _ := k.item(service(t, a.ConfigDir)); v != somebody {
			t.Fatal("and leaves it")
		}
	})
}

/* --------------------------------------------------------------- smaller --- */

// Where rota keeps no login in a home — a home whose path names no keychain
// item on macOS, or Windows — it has none to remove there, and nothing that
// runs there holds one of rota's: removing or moving the account deletes no
// credential file, and a move is not refused.
func TestWhereRotaKeepsNoLoginItRemovesNone(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	fakeKeychain(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	dir := filepath.Join(t.TempDir(), "projét")
	a.ConfigDir = dir
	writeFile(t, filepath.Join(dir, ".credentials.json"), storeLogin("A-x", "R-x", later(time.Hour), "1"))
	alive(t, dir, "sessions/1.json", os.Getpid())
	if err := s.MoveHome(context.Background(), a, t.TempDir()); err != nil {
		t.Fatalf("not refused: %v", err)
	}
	if readLogin2(t, dir) != "R-x" {
		t.Fatal("a move deletes no credential file")
	}
	a.ConfigDir = dir
	os.Remove(filepath.Join(dir, "sessions", "1.json"))
	if err := s.Remove(a.ID); err != nil {
		t.Fatal(err)
	}
	if readLogin2(t, a.ConfigDir) != "R-x" {
		t.Fatal("no credential file deleted")
	}
}

// A home still holding the very login rota refreshed away is written even
// when something has started there meanwhile: that login is spent, and every
// window on it would be signed out. Here a window starts between the save
// and the write.
func TestASpentLoginRotaRefreshedAwayIsWrittenOverEvenWhileSomethingRuns(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	// The home holds what rota wrote, its access token lapsed as the account's.
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", later(-time.Hour), "1999999999000"))
	expire(a)
	ob.watch(s, a, "R-new")
	ob.then = func() { alive(t, s.Home(a), "sessions/9.json", os.Getpid()) }
	if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	ob.savedThenWritten(t)

	// And a launch that finds the spent login with something alive writes too.
	b := livingClaude(s, "u2")
	if _, err := launchEnv(t, s, b); err != nil {
		t.Fatal(err)
	}
	b.Token.Refresh = "R-u2-next" // rota refreshed it, and the write never happened
	alive(t, s.Home(b), "sessions/1.json", os.Getpid())
	if _, err := launchEnv(t, s, b); err != nil {
		t.Fatal(err)
	}
	if readLogin2(t, s.Home(b)) != "R-u2-next" {
		t.Fatal("the spent login is replaced")
	}
}

// Removing an account makes its home quiet before it asks whether a run
// holds the account, so a daemon that held the claim would be stopped rather
// than refuse the removal.
func TestRemovingStopsTheDaemonBeforeItAsksAboutTheClaim(t *testing.T) {
	claudeWorld(t)
	d := fakeDaemons(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	release, ok := s.holdRun(a)
	if !ok {
		t.Fatal("claim")
	}
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	d.then = func(home string) {
		stopsClean(home)
		release() // the daemon held it, and let go as it stopped
	}
	if err := s.Remove(a.ID); err != nil {
		t.Fatalf("stopped, then removed: %v", err)
	}
	if d.stops() != 1 {
		t.Fatal("the daemon was stopped")
	}
}

// A store whose lock a run has released tries no claim at all: a moment's
// exclusive try is enough to refuse a launch's shared one.
func TestAReleasedStoreTriesNoClaim(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	c := s.add("codex")
	c.Token = rota.Token{Access: "c", Refresh: "rc"}
	a.Long = &rota.LongToken{Access: "LONG", ExpiresAt: later(300 * 24 * time.Hour)}
	_, release, err := s.ready(context.Background(), a, false)
	if err != nil {
		t.Fatal(err)
	}
	release()
	expire(a)
	var tries atomic.Int64
	oldX, oldS := tryExclusive, tryShared
	tryExclusive = func(p string) (*os.File, bool, error) { tries.Add(1); return oldX(p) }
	tryShared = func(p string) (*os.File, bool, error) { tries.Add(1); return oldS(p) }
	t.Cleanup(func() { tryExclusive, tryShared = oldX, oldS })
	s.Refresh(context.Background(), true)
	s.Maintain(context.Background())
	if n := tries.Load(); n != 0 || f.refreshes.Load() != 0 {
		t.Fatalf("claims tried %d, refreshes %d", n, f.refreshes.Load())
	}
}

// A launch that goes onto the stored login while the daemon started on a
// token stays — something else runs there, so it is not stopped — says
// that background sessions started meanwhile use that daemon's credential.
func TestADaemonFromTheOtherRouteThatStaysIsSaidSo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	d := fakeDaemons(t)
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"), storeLogin("A-u1", "R-u1", a.Token.ExpiresAt, "1999999999000"))
	alive(t, s.Home(a), "daemon.lock", os.Getpid())
	alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	env, err := launchEnv(t, s, a)
	if err != nil || hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || d.stops() != 0 {
		t.Fatalf("the stored route, the daemon left: %v %v %d", err, env, d.stops())
	}
	found := false
	for _, w := range *words {
		found = found || strings.Contains(w, "old credential until the home goes quiet")
	}
	if !found {
		t.Fatalf("said: %q", *words)
	}
}

// A keychain that cannot be read says what a person can do about it.
func TestAnUnreadableKeychainSaysWhatToDo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	k.findCode = 51
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if len(*words) != 1 || !strings.Contains((*words)[0], "unlock the login keychain") || !strings.Contains((*words)[0], "rota login --long") {
		t.Fatalf("said: %q", *words)
	}
}
