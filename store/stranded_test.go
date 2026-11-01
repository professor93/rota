package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// stranded leaves a home the way Claude Code's daemon leaves it when its
// refresh failed after the access token had run out: the account's own login
// in the store, expired an hour ago, the daemon alive, and a status file
// saying it gave up half an hour ago.
func stranded(t *testing.T, s *Store, a *rota.Account) string {
	t.Helper()
	home := s.Home(a)
	a.Token.ExpiresAt = later(-time.Hour)
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
	a.StagedWritten()
	alive(t, home, "daemon.lock", os.Getpid())
	gaveUp(t, home, later(-30*time.Minute))
	return home
}

// gaveUp writes the status Claude Code's daemon leaves behind.
func gaveUp(t *testing.T, home string, since int64) {
	t.Helper()
	writeFile(t, filepath.Join(home, "daemon-auth-status.json"), `{"status":"auth_required","since":`+strconv.FormatInt(since, 10)+`}`)
}

// looked reads the home the way every path through a stranded home does.
func looked(t *testing.T, s *Store, a *rota.Account) *claudeHome {
	t.Helper()
	h := s.claudeHome(a, s.claimed(a))
	if err := h.adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func saidOf(said *[]string, word string) string {
	for _, m := range *said {
		if strings.Contains(m, word) {
			return m
		}
	}
	return ""
}

// A daemon that gave up leaves the home waiting for somebody to refresh, and
// rota waited for the daemon to go away: an account sat like that for days,
// on a login that was alive the whole time. Every path that may refresh ends
// it: the login is refreshed, saved, and written into the home under the
// running daemon, whose re-check picks it up. The daemon is left alone, and
// the person is told in one line.
func TestAStrandedHomeIsRefreshedAndSeededWhileItsDaemonIsAlive(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, c := range []struct {
		what string
		do   func(t *testing.T, s *Store, a *rota.Account)
	}{
		{"maintenance", func(t *testing.T, s *Store, a *rota.Account) {
			if errs := s.Maintain(context.Background()); len(errs) != 0 {
				t.Fatal(errs)
			}
		}},
		{"a listing", func(t *testing.T, s *Store, a *rota.Account) {
			if errs := s.Refresh(context.Background(), true, a); len(errs) != 0 {
				t.Fatal(errs)
			}
		}},
		{"a launch", func(t *testing.T, s *Store, a *rota.Account) {
			env, err := launchEnv(t, s, a)
			if err != nil {
				t.Fatal(err)
			}
			if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Fatalf("a launch stays on the stored login: %v", env)
			}
			if a.Extra[routeKey] != routeStored {
				t.Fatal("and remembers it")
			}
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			f.refreshes.Store(0)
			s, ob := orderedStore(t)
			words := said(s)
			a := livingClaude(s, "u1")
			home := stranded(t, s, a)
			ob.watch(s, a, "R-new")
			c.do(t, s, a)
			if f.refreshes.Load() != 1 {
				t.Fatalf("one refresh: %d", f.refreshes.Load())
			}
			if a.Token.Refresh != "R-new" || a.Dead {
				t.Fatalf("the account holds the new login: %+v dead=%v", a.Token, a.Dead)
			}
			if r, _ := readLogin(t, home); r != "R-new" {
				t.Fatalf("and so does the home: %q", r)
			}
			ob.savedThenWritten(t)
			if !a.StagedCurrent() {
				t.Fatal("rota knows the home holds it")
			}
			if _, err := os.Stat(filepath.Join(home, "daemon.lock")); err != nil {
				t.Fatalf("the daemon is left alone: %v", err)
			}
			line := saidOf(words, "gave up") + saidOf(words, "given up")
			if !strings.Contains(line, a.String()) || !strings.Contains(line, "refreshed") {
				t.Fatalf("one line names the account and what happened: %q", *words)
			}
		})
	}
}

// Stranded is precise: every condition is needed, and each of these is a home
// rota must go on leaving alone.
func TestAHomeIsStrandedOnlyWhenEverythingSaysSo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, c := range []struct {
		what  string
		do    func(t *testing.T, s *Store, a *rota.Account, home string)
		wants bool
	}{
		{"all of it", func(*testing.T, *Store, *rota.Account, string) {}, true},
		{"the status file is missing", func(t *testing.T, _ *Store, _ *rota.Account, home string) {
			os.Remove(filepath.Join(home, "daemon-auth-status.json"))
		}, false},
		{"it says something else", func(t *testing.T, _ *Store, _ *rota.Account, home string) {
			writeFile(t, filepath.Join(home, "daemon-auth-status.json"), `{"status":"ok","since":`+strconv.FormatInt(later(-time.Minute), 10)+`}`)
		}, false},
		{"it cannot be read", func(t *testing.T, _ *Store, _ *rota.Account, home string) {
			writeFile(t, filepath.Join(home, "daemon-auth-status.json"), `{"status":"auth_req`)
		}, false},
		{"it says when nobody knows", func(t *testing.T, _ *Store, _ *rota.Account, home string) {
			writeFile(t, filepath.Join(home, "daemon-auth-status.json"), `{"status":"auth_required"}`)
		}, false},
		{"it is older than the token that ran out", func(t *testing.T, _ *Store, _ *rota.Account, home string) {
			gaveUp(t, home, later(-3*tokenLife))
		}, false},
		{"the token is still good", func(t *testing.T, s *Store, a *rota.Account, home string) {
			a.Token.ExpiresAt = later(time.Hour)
			writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
		}, false},
		{"it ran out too recently", func(t *testing.T, s *Store, a *rota.Account, home string) {
			a.Token.ExpiresAt = later(time.Minute - strandedGrace)
			writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
		}, false},
		{"the account is dead", func(t *testing.T, s *Store, a *rota.Account, home string) {
			a.Dead, a.DeadReason = true, "refused"
		}, false},
		{"the store lock was released", func(t *testing.T, s *Store, a *rota.Account, home string) {
			s.released = true
		}, false},
		{"no daemon is alive", func(t *testing.T, s *Store, a *rota.Account, home string) {
			alive(t, home, "daemon.lock", deadPID)
		}, false},
		{"its lock cannot be read", func(t *testing.T, s *Store, a *rota.Account, home string) {
			writeFile(t, filepath.Join(home, "daemon.lock"), `{"pid":`)
		}, false},
		{"a window is open beside it", func(t *testing.T, s *Store, a *rota.Account, home string) {
			alive(t, home, "sessions/1.json", os.Getpid())
		}, false},
		{"another rota run holds the account", func(t *testing.T, s *Store, a *rota.Account, home string) {
			release, ok := s.holdRun(a)
			if !ok {
				t.Fatal("the claim")
			}
			t.Cleanup(release)
		}, false},
		{"only what it hosts is beside it", func(t *testing.T, s *Store, a *rota.Account, home string) {
			hosted(t, home, "sessions/1.json", os.Getpid())
		}, true},
		{"the home holds a login that is not the account's", func(t *testing.T, s *Store, a *rota.Account, home string) {
			a.StagedSuperseded()
			writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-old", "R-old", later(-48*time.Hour), "1999999999000"))
		}, false},
		{"the home holds no login", func(t *testing.T, s *Store, a *rota.Account, home string) {
			os.Remove(filepath.Join(home, ".credentials.json"))
		}, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := openTemp(t)
			a := livingClaude(s, "u1")
			home := stranded(t, s, a)
			c.do(t, s, a, home)
			if got := looked(t, s, a).stranded(); got != c.wants {
				t.Fatalf("stranded=%v", got)
			}
		})
	}
	t.Run("in the hold", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		stranded(t, s, a)
		h := looked(t, s, a)
		h.hold = "something rota cannot place"
		if h.stranded() {
			t.Fatal("the hold decides first")
		}
	})
	t.Run("a chosen directory", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		a.ConfigDir = t.TempDir()
		stranded(t, s, a)
		if s.claudeHome(a, false).stranded() {
			t.Fatal("rota keeps no login there")
		}
	})
}

// The status file stays auth_required long after recovery, so on its own it
// means nothing: with a good token nothing is refreshed and nothing written.
// Nor does it mean anything for a token that ran out after it was written,
// which is a healthy daemon that has not got to its refresh yet — a laptop
// that slept through the expiry, say.
func TestAStaleStatusFileDoesNothing(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, c := range []struct {
		what string
		do   func(t *testing.T, a *rota.Account, home string)
	}{
		{"with a good token", func(t *testing.T, a *rota.Account, home string) {
			a.Token.ExpiresAt = later(time.Hour)
			writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
		}},
		{"from before the token ran out and the token before it", func(t *testing.T, a *rota.Account, home string) {
			gaveUp(t, home, later(-3*tokenLife))
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := openTemp(t)
			a := livingClaude(s, "u1")
			home := stranded(t, s, a)
			c.do(t, a, home)
			before := readFile(filepath.Join(home, ".credentials.json"))
			if _, err := launchEnv(t, s, a); err != nil {
				t.Fatal(err)
			}
			s.Maintain(context.Background())
			s.Refresh(context.Background(), true, a)
			if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
				t.Fatalf("nothing refreshed: %d %q", f.refreshes.Load(), a.Token.Refresh)
			}
			if readFile(filepath.Join(home, ".credentials.json")) != before {
				t.Fatal("nothing written")
			}
		})
	}
}

// The rule stands everywhere else: with a window open beside the daemon, that
// window is a refresher that has not given up, and nothing changes.
func TestAWindowBesideAGivenUpDaemonKeepsTheHomeItsOwn(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	alive(t, home, "sessions/1.json", os.Getpid())
	s.Maintain(context.Background())
	s.Refresh(context.Background(), true, a)
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatalf("nothing refreshed: %d %q", f.refreshes.Load(), a.Token.Refresh)
	}
	if r, _ := readLogin(t, home); r != "R-u1" {
		t.Fatalf("nothing written: %q", r)
	}
}

// A refusal goes the way every refusal goes: the home is read again first,
// so a sibling that refreshed in the meantime is taken and the account lives;
// otherwise the account is dead, the refused token is never presented again,
// and nothing is said about a rescue that did not happen.
func TestARefusedRescueGoesTheWayEveryRefusalGoes(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)

	t.Run("refused for good", func(t *testing.T) {
		f.refreshes.Store(0)
		f.set(func(f *anthropic) {
			f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
		})
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		home := stranded(t, s, a)
		before := readFile(filepath.Join(home, ".credentials.json"))
		s.Maintain(context.Background())
		if !a.Dead || !strings.Contains(a.DeadReason, "invalid_grant") {
			t.Fatalf("dead, and says why: %v %q", a.Dead, a.DeadReason)
		}
		if f.refreshes.Load() != 1 || readFile(filepath.Join(home, ".credentials.json")) != before {
			t.Fatalf("presented once, nothing written: %d", f.refreshes.Load())
		}
		s.Maintain(context.Background())
		s.Refresh(context.Background(), true, a)
		if f.refreshes.Load() != 1 {
			t.Fatalf("and never presented again: %d", f.refreshes.Load())
		}
		if line := saidOf(words, "given up") + saidOf(words, "gave up"); line != "" {
			t.Fatalf("no rescue happened: %q", line)
		}
	})
	t.Run("a sibling got there first", func(t *testing.T) {
		f.refreshes.Store(0)
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		home := stranded(t, s, a)
		f.set(func(f *anthropic) {
			f.profile = profileOf("A-sib", "u1")
			f.refresh = func() (int, any) {
				writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-sib", "R-sib", later(time.Hour), "1888888888000"))
				return 400, map[string]string{"error": "invalid_grant"}
			}
		})
		s.Maintain(context.Background())
		if a.Dead || a.Token.Refresh != "R-sib" {
			t.Fatalf("the sibling's login is taken: dead=%v %q", a.Dead, a.Token.Refresh)
		}
		if r, _ := readLogin(t, home); r != "R-sib" {
			t.Fatalf("and the home is left as the sibling left it: %q", r)
		}
		if line := saidOf(words, "given up") + saidOf(words, "gave up"); line != "" {
			t.Fatalf("no rescue happened: %q", line)
		}
	})
}

// A rescue that does not get through changes nothing: the account stays
// alive, and a launch joins the home as it always did rather than failing.
// The window it starts refreshes for itself.
func TestARescueThatDoesNotGetThroughFailsNoLaunch(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
	})
	s := openTemp(t)
	words := said(s)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	env, err := launchEnv(t, s, a)
	if err != nil {
		t.Fatalf("the launch joins the home: %v", err)
	}
	if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") || a.Dead || a.Token.Refresh != "R-u1" {
		t.Fatalf("nothing changed: %v dead=%v %q", env, a.Dead, a.Token.Refresh)
	}
	if r, _ := readLogin(t, home); r != "R-u1" {
		t.Fatalf("nothing written: %q", r)
	}
	if !strings.Contains(strings.Join(*words, "\n"), a.String()) {
		t.Fatalf("one line says so: %q", *words)
	}
}

// A refresh that keeps the refresh token still replaced the access token,
// and that is what the daemon is waiting for: it goes into the home too, or
// every look would spend another refresh on the same stranded home.
func TestARescueThatKeepsTheRefreshTokenStillReachesTheHome(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-u1") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(readFile(filepath.Join(home, ".credentials.json")), "A-new") {
		t.Fatal("the new access token is in the home")
	}
	if !a.StagedCurrent() {
		t.Fatal("rota knows it")
	}
	s.Maintain(context.Background())
	if f.refreshes.Load() != 1 {
		t.Fatalf("one refresh, once: %d", f.refreshes.Load())
	}
}

// On macOS the item is what the daemon reads first, so the rescue takes it
// out of the way as any write does: the file goes in, the item goes, and the
// daemon's next read finds the file.
func TestARescueOnMacOSTakesTheKeychainItemOutOfTheWay(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	svc := service(t, home)
	k.items[svc] = readFile(filepath.Join(home, ".credentials.json"))
	os.Remove(filepath.Join(home, ".credentials.json"))
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("the item is gone")
	}
	if r, _ := readLogin(t, home); r != "R-new" {
		t.Fatalf("the file holds the new login: %q", r)
	}
}

// While rota refreshes a stranded home and writes the login in, it holds the
// lock Claude Code's own processes take around a refresh, so the daemon's
// re-check cannot present the same refresh token beside it, and finds the new
// login when it gets the lock. The lock goes when the login is in the home.
func TestARescueHoldsClaudeCodesRefreshLock(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	for _, c := range []struct {
		what string
		do   func(s *Store, a *rota.Account)
	}{
		{"maintenance", func(s *Store, a *rota.Account) { s.Maintain(context.Background()) }},
		{"a listing", func(s *Store, a *rota.Account) { s.Refresh(context.Background(), true, a) }},
		{"a launch", func(s *Store, a *rota.Account) { launchEnv(t, s, a) }},
	} {
		t.Run(c.what, func(t *testing.T) {
			s, ob := orderedStore(t)
			a := livingClaude(s, "u1")
			home := stranded(t, s, a)
			lock := filepath.Join(home, claudeRefreshLock)
			held := false
			f.set(func(f *anthropic) {
				inner := refreshesTo("A-new", "R-new")
				f.refresh = func() (int, any) {
					_, err := os.Stat(lock)
					held = err == nil
					return inner()
				}
			})
			ob.watch(s, a, "R-new")
			var whileSaved error
			ob.then = func() { _, whileSaved = os.Stat(lock) }
			c.do(s, a)
			if !held {
				t.Fatal("held while the token is presented")
			}
			if whileSaved != nil {
				t.Fatalf("and still held once the store has it: %v", whileSaved)
			}
			if r, _ := readLogin(t, home); r != "R-new" {
				t.Fatalf("the home holds it: %q", r)
			}
			if _, err := os.Stat(lock); !os.IsNotExist(err) {
				t.Fatalf("and the lock is gone: %v", err)
			}
		})
	}
}

// A lock somebody holds means Claude Code is refreshing right now, so this
// look leaves it to them. One left behind by a process that died, which
// nobody has touched for a minute, is taken, as Claude Code takes it.
func TestARescueWaitsForAClaudeCodeThatIsRefreshing(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	lock := filepath.Join(home, claudeRefreshLock)
	if err := os.Mkdir(lock, 0o700); err != nil {
		t.Fatal(err)
	}
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	s.Refresh(context.Background(), true, a)
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-u1" {
		t.Fatalf("left to whoever holds it: %d %q", f.refreshes.Load(), a.Token.Refresh)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("and that lock is not rota's to touch: %v", err)
	}
	old := time.Now().Add(-2 * refreshLockStale)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 || a.Token.Refresh != "R-new" {
		t.Fatalf("a dead holder's lock is taken: %d %q", f.refreshes.Load(), a.Token.Refresh)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("and let go: %v", err)
	}
}

// The home is read again under the lock: a daemon that got through a moment
// before is taken, with no refresh of rota's beside it.
func TestARescueReadsTheHomeAgainOnceItHoldsTheLock(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	h := looked(t, s, a)
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-own", "R-own", later(8*time.Hour), "1999999999000"))
	if _, err := h.renew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.refreshes.Load() != 0 || a.Token.Refresh != "R-own" {
		t.Fatalf("the daemon's own refresh is taken: %d %q", f.refreshes.Load(), a.Token.Refresh)
	}
}

// A provider that cannot be reached leaves a listing with nothing worse than
// the error it reports for any account, and the account as it was.
func TestAListingReportsARescueThatDidNotGetThrough(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
	})
	s := openTemp(t)
	a := livingClaude(s, "u1")
	stranded(t, s, a)
	errs := s.Refresh(context.Background(), true, a)
	if len(errs) != 1 || a.Dead || errors.Is(errs[0], rota.ErrReauth) {
		t.Fatalf("%v dead=%v", errs, a.Dead)
	}
}
