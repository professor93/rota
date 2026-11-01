package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// clearedLogin is a credential store as Claude Code leaves it when a refresh of
// its own comes back refused, measured on a real keychain item: the tokens and
// the expiry blanked, everything else kept, the secrets beside the login too.
const clearedLogin = `{"mcpOAuth":{"srv":{"accessToken":"mcp-secret"}},"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,` +
	`"scopes":["user:inference"],"subscriptionType":"max","clientId":"client"}}`

// cleared leaves a home whose current login Claude Code has blanked.
func cleared(t *testing.T, s *Store, a *rota.Account) string {
	t.Helper()
	home := s.Home(a)
	a.Token.ExpiresAt = later(-time.Hour)
	writeFile(t, filepath.Join(home, ".credentials.json"), clearedLogin)
	a.StagedWritten()
	return home
}

// everyLook is every way rota comes to look at a home, each of them asking for
// nothing else: a listing with a reading a moment old, a listing that asks no
// provider anything, maintenance and a launch.
func everyLook() []struct {
	what string
	do   func(t *testing.T, s *Store, a *rota.Account)
} {
	noErrs := func(t *testing.T, errs []error) {
		t.Helper()
		if len(errs) != 0 {
			t.Fatal(errs)
		}
	}
	return []struct {
		what string
		do   func(t *testing.T, s *Store, a *rota.Account)
	}{
		{"maintenance", func(t *testing.T, s *Store, a *rota.Account) { noErrs(t, s.Maintain(context.Background())) }},
		{"a listing with a fresh reading", func(t *testing.T, s *Store, a *rota.Account) {
			a.QuotaAt = rota.NowMS()
			noErrs(t, s.Refresh(context.Background(), false, a))
		}},
		{"a listing that asks nothing", func(t *testing.T, s *Store, a *rota.Account) { noErrs(t, s.Rescue(context.Background(), a)) }},
		{"a launch", func(t *testing.T, s *Store, a *rota.Account) {
			env, err := launchEnv(t, s, a)
			if err != nil {
				t.Fatal(err)
			}
			if hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Fatalf("it stays on the stored login: %v", env)
			}
		}},
	}
}

// homes are the states a rescue must not care about: an open window for days is
// what the owner reports, and a claim is what such a window holds.
func homesAround() []struct {
	what string
	make func(t *testing.T, s *Store, a *rota.Account, home string)
} {
	return []struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account, home string)
	}{
		{"a daemon", func(t *testing.T, s *Store, a *rota.Account, home string) { alive(t, home, "daemon.lock", os.Getpid()) }},
		{"a window", func(t *testing.T, s *Store, a *rota.Account, home string) {
			alive(t, home, "sessions/1.json", os.Getpid())
		}},
		{"a run holding the claim", func(t *testing.T, s *Store, a *rota.Account, home string) {
			release, ok := s.holdRun(a)
			if !ok {
				t.Fatal("the claim")
			}
			t.Cleanup(release)
		}},
	}
}

// A window open for days keeps a stranded home from nothing: it has had its
// chance, the token is well past its expiry and the daemon has said so. What
// the owner reports is exactly this, so neither a window nor a held claim may
// stand in the way of any look, and a reading that is still fresh or a listing
// that asks no provider anything may not either.
func TestAStrandedHomeIsRescuedWhateverRunsBesideItsDaemon(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, look := range everyLook() {
		for _, around := range homesAround() {
			t.Run(look.what+" beside "+around.what, func(t *testing.T) {
				f.refreshes.Store(0)
				s, ob := orderedStore(t)
				a := livingClaude(s, "u1")
				home := stranded(t, s, a)
				around.make(t, s, a, home)
				ob.watch(s, a, "R-new")
				look.do(t, s, a)
				if f.refreshes.Load() != 1 || a.Token.Refresh != "R-new" {
					t.Fatalf("one refresh: %d %q", f.refreshes.Load(), a.Token.Refresh)
				}
				ob.savedThenWritten(t)
			})
		}
	}
}

// A daemon's report goes on meaning what it meant after the daemon has gone, if
// something is still there to be stuck on that login.
func TestAWindowLeftBehindByAGivenUpDaemonIsStrandedToo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	alive(t, home, "daemon.lock", deadPID)
	alive(t, home, "sessions/1.json", os.Getpid())
	if !looked(t, s, a).stranded() {
		t.Fatal("stranded")
	}
}

// The login Claude Code blanked goes back, whatever is or is not running there
// and with no report from any daemon, because Claude Code's own verdict is not
// proof of a refusal: rota's refresh has worked where its daemon's had not. No
// account is called dead on the way, in memory or on disk, and the secrets
// Claude Code keeps beside the login stay.
func TestABlankedStoreIsRescuedBeforeTheAccountIsCalledDead(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	around := append(homesAround(), struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account, home string)
	}{"nothing", func(*testing.T, *Store, *rota.Account, string) {}})
	for _, look := range everyLook() {
		for _, c := range around {
			t.Run(look.what+" beside "+c.what, func(t *testing.T) {
				f.refreshes.Store(0)
				s, ob := orderedStore(t)
				words := said(s)
				a := livingClaude(s, "u1")
				home := cleared(t, s, a)
				c.make(t, s, a, home)
				ob.watch(s, a, "R-new")
				look.do(t, s, a)
				if f.refreshes.Load() != 1 || a.Dead || a.Token.Refresh != "R-new" {
					t.Fatalf("one refresh and alive: %d dead=%v %q", f.refreshes.Load(), a.Dead, a.Token.Refresh)
				}
				ob.savedThenWritten(t)
				if ob.dead {
					t.Fatal("nothing says it was ever dead")
				}
				r, doc := readLogin(t, home)
				if r != "R-new" || !strings.Contains(readFile(filepath.Join(home, ".credentials.json")), "mcp-secret") {
					t.Fatalf("the login is back beside what Claude Code kept: %q %v", r, doc)
				}
				if line := strings.Join(*words, "\n"); !strings.Contains(line, a.String()) || !strings.Contains(line, "cleared") {
					t.Fatalf("one line says so: %q", *words)
				}
				if !a.StagedCurrent() {
					t.Fatal("rota knows the home holds it")
				}
				if err := s.Maintain(context.Background()); len(err) != 0 || f.refreshes.Load() != 1 {
					t.Fatalf("and that was once: %v %d", err, f.refreshes.Load())
				}
			})
		}
	}
}

// An access token rota still counts as good says nothing here, because Claude
// Code blanks a login on a refresh it was forced into: the token is presented
// anyway, or a refused login would be written in again and again.
func TestABlankedStoreIsRescuedEvenBeforeTheTokenExpires(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := cleared(t, s, a)
	good := later(time.Hour)
	a.Token.ExpiresAt = good
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
	})
	s.Maintain(context.Background())
	if f.refreshes.Load() != 1 {
		t.Fatalf("presented: %d", f.refreshes.Load())
	}
	if a.Token.ExpiresAt != good {
		t.Fatalf("a look that failed changes nothing: %d", a.Token.ExpiresAt)
	}
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s.Maintain(context.Background())
	if r, _ := readLogin(t, home); r != "R-new" {
		t.Fatalf("the next look gets through: %q", r)
	}
}

// A refusal of rota's own token is the end of it: the account is dead, says why
// in words a person can read, and that token is never presented again. Nothing
// is written.
func TestARefusedRescueOfABlankedStoreIsFinal(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
	})
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := cleared(t, s, a)
	s.Maintain(context.Background())
	if !a.Dead || !strings.Contains(a.WhyDead(), "cleared") || !strings.Contains(a.WhyDead(), "invalid_grant") {
		t.Fatalf("dead, and says why: %v %q", a.Dead, a.WhyDead())
	}
	if f.refreshes.Load() != 1 || readFile(filepath.Join(home, ".credentials.json")) != clearedLogin {
		t.Fatalf("presented once, nothing written: %d", f.refreshes.Load())
	}
	s.Maintain(context.Background())
	s.Refresh(context.Background(), true, a)
	s.Rescue(context.Background(), a)
	if _, err := launchEnv(t, s, a); !errors.Is(err, rota.ErrReauth) {
		t.Fatalf("a launch is refused: %v", err)
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("and it is never presented again: %d", f.refreshes.Load())
	}
}

// A refusal is on disk before the look is over, or another process would
// present the token again.
func TestARefusalOfABlankedStoreIsSavedAtOnce(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
	})
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	cleared(t, s, a)
	s.Refresh(context.Background(), false, a)
	if !ob.dead {
		t.Fatal("saved")
	}
}

// A provider that cannot be reached says nothing about the login: the account
// stays alive and says so, and the next look tries again.
func TestABlankedStoreThatCannotBeRescuedYetStaysAlive(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
	})
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	home := cleared(t, s, a)
	errs := s.Refresh(context.Background(), false, a)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), a.String()) || !strings.Contains(errs[0].Error(), "cleared") {
		t.Fatalf("says so: %v", errs)
	}
	if a.Dead || ob.dead || a.Token.Refresh != "R-u1" || readFile(filepath.Join(home, ".credentials.json")) != clearedLogin {
		t.Fatalf("alive and as it was: %v %q", a.Dead, a.Token.Refresh)
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("one try a look: %d", f.refreshes.Load())
	}
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	if errs := s.Refresh(context.Background(), false, a); len(errs) != 0 {
		t.Fatal(errs)
	}
	if r, _ := readLogin(t, home); r != "R-new" || a.Dead {
		t.Fatalf("the next look gets through: %q %v", r, a.Dead)
	}
}

// With no login in the home a launch has nothing to join. Claude Code started
// there would not be signed in, so it goes on a token that is good, or does not
// go, and says what is happening.
func TestALaunchIntoABlankedStoreThatCannotBeRescuedNeverStartsSignedOut(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 503, map[string]string{"error": "overloaded"} }
	})
	t.Run("no good token", func(t *testing.T) {
		s := openTemp(t)
		words := said(s)
		a := livingClaude(s, "u1")
		cleared(t, s, a)
		_, err := launchEnv(t, s, a)
		if !errors.Is(err, rota.ErrBusy) || !strings.Contains(err.Error(), "cleared") || a.Dead {
			t.Fatalf("%v dead=%v", err, a.Dead)
		}
		if !strings.Contains(strings.Join(*words, "\n"), a.String()) {
			t.Fatalf("one line says so: %q", *words)
		}
	})
	t.Run("a long-lived token", func(t *testing.T) {
		s := openTemp(t)
		a := livingClaude(s, "u1")
		cleared(t, s, a)
		a.ApplyLong(&rota.Token{Access: "L", ExpiresAt: later(1000 * time.Hour)})
		env, err := launchEnv(t, s, a)
		if err != nil || !hasVar(env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("%v %v", err, env)
		}
	})
}

// A blanked login that is not the account's current one is what it always was:
// a home with nothing usable in it. The account owes it nothing, is not dead,
// and its login goes in when rota may write.
func TestABlankedOlderLoginIsNoRescue(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	writeFile(t, filepath.Join(home, ".credentials.json"), clearedLogin)
	a.StagedSuperseded()
	alive(t, home, "sessions/1.json", os.Getpid())
	s.Maintain(context.Background())
	s.Rescue(context.Background(), a)
	if f.refreshes.Load() != 0 || a.Dead {
		t.Fatalf("%d %v", f.refreshes.Load(), a.Dead)
	}
	if readFile(filepath.Join(home, ".credentials.json")) != clearedLogin {
		t.Fatal("nothing written while something runs there")
	}
}

// A store whose lock was released rotates nothing, here as everywhere, and does
// not call anybody dead for what it will not try.
func TestAReleasedStoreLeavesABlankedHomeAlone(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	cleared(t, s, a)
	if err := s.Release(); err != nil {
		t.Fatal(err)
	}
	s.Refresh(context.Background(), true, a)
	s.Rescue(context.Background(), a)
	if f.refreshes.Load() != 0 || a.Dead {
		t.Fatalf("%d %v", f.refreshes.Load(), a.Dead)
	}
}

/* ---------------------------------------------------------------- the lock --- */

// Claude Code calls the lock stale after a minute with its mtime untouched, and
// then takes it and presents the token rota is presenting, so rota keeps it
// fresh the way Claude Code's own holders do for as long as it holds it.
func TestTheRefreshLockIsKeptFreshForAsLongAsItIsHeld(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	old := refreshLockBeat
	refreshLockBeat = 10 * time.Millisecond
	t.Cleanup(func() { refreshLockBeat = old })
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	lock := filepath.Join(home, claudeRefreshLock)
	var first, last time.Time
	f.set(func(f *anthropic) {
		inner := refreshesTo("A-new", "R-new")
		f.refresh = func() (int, any) {
			fi, err := os.Stat(lock)
			if err != nil {
				t.Error(err)
				return inner()
			}
			first = fi.ModTime()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if fi, err := os.Stat(lock); err == nil && fi.ModTime().After(first) {
					last = fi.ModTime()
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			return inner()
		}
	})
	s.Maintain(context.Background())
	if !last.After(first) {
		t.Fatalf("its mtime moved while the provider was asked: %v %v", first, last)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("and it goes with the rescue: %v", err)
	}
}

// Somebody who judged the lock stale and took it now owns it, whatever rota was
// doing. rota stops touching it and leaves it where it is.
func TestALockSomebodyElseTookIsNotRotas(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	old := refreshLockBeat
	refreshLockBeat = 10 * time.Millisecond
	t.Cleanup(func() { refreshLockBeat = old })
	f := fakeAnthropic(t)
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	lock := filepath.Join(home, claudeRefreshLock)
	theirs := time.Now().Add(-10 * time.Second).Truncate(time.Second)
	f.set(func(f *anthropic) {
		inner := refreshesTo("A-new", "R-new")
		f.refresh = func() (int, any) {
			if err := os.Remove(lock); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(lock, 0o700); err != nil {
				t.Error(err)
			}
			if err := os.Chtimes(lock, theirs, theirs); err != nil {
				t.Error(err)
			}
			time.Sleep(60 * time.Millisecond) // long enough for several beats
			return inner()
		}
	})
	s.Maintain(context.Background())
	fi, err := os.Stat(lock)
	if err != nil {
		t.Fatalf("the lock that replaced rota's stays: %v", err)
	}
	if !fi.ModTime().Equal(theirs) {
		t.Fatalf("and rota did not touch it: %v, want %v", fi.ModTime(), theirs)
	}
}

// One sequence per account: a rescue's refresh, save and write are over, and its
// lock gone, before the next account's starts, so a slow account holds nobody
// else's home open.
func TestARescuedAccountHoldsItsLockForItsOwnSequenceOnly(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	s := openTemp(t)
	a, b := livingClaude(s, "u1"), livingClaude(s, "u2")
	homeA, homeB := stranded(t, s, a), stranded(t, s, b)
	var n atomic.Int32
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) {
			held := 0
			for _, home := range []string{homeA, homeB} {
				if _, err := os.Stat(filepath.Join(home, claudeRefreshLock)); err == nil {
					held++
				}
			}
			if held != 1 {
				t.Errorf("one lock at a time, held %d", held)
			}
			i := strconv.Itoa(int(n.Add(1)))
			return refreshesTo("A-"+i, "R-new"+i)()
		}
	})
	if errs := s.Refresh(context.Background(), true, a, b); len(errs) != 0 {
		t.Fatal(errs)
	}
	if n.Load() != 2 || !a.StagedCurrent() || !b.StagedCurrent() {
		t.Fatalf("both: %d", n.Load())
	}
}

// A panic in the middle of a rescue must not leave Claude Code locked out of its
// own refresh for a minute: the lock goes on every path.
func TestAPanicInARescueLetsGoOfTheLock(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, c := range []struct {
		what string
		do   func(s *Store, a *rota.Account)
	}{
		{"a listing", func(s *Store, a *rota.Account) { s.Refresh(context.Background(), true, a) }},
		{"a launch", func(s *Store, a *rota.Account) {
			defer func() { _ = recover() }()
			launchEnv(t, s, a)
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s, ob := orderedStore(t)
			a := livingClaude(s, "u1")
			home := stranded(t, s, a)
			ob.watch(s, a, "R-new")
			ob.then = func() { panic("in the middle of a rescue") }
			c.do(s, a)
			if !ob.saved {
				t.Fatal("the panic happened")
			}
			if _, err := os.Stat(filepath.Join(home, claudeRefreshLock)); !os.IsNotExist(err) {
				t.Fatalf("the lock is gone: %v", err)
			}
		})
	}
}

/* --------------------------------------------- once the provider has answered --- */

// onHomeWrites puts fail in front of the last step of every write of a home's
// credential store, and leaves rota's own store alone.
func onHomeWrites(t *testing.T, fail func(n int) error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	old := renameFile
	renameFile = func(from, to string) error {
		if filepath.Base(to) == ".credentials.json" {
			if err := fail(int(calls.Add(1))); err != nil {
				return err
			}
		}
		return old(from, to)
	}
	t.Cleanup(func() { renameFile = old })
	return &calls
}

// failRenames makes every write of a home's credential store fail at its last
// step.
func failRenames(t *testing.T) *atomic.Int32 {
	t.Helper()
	return onHomeWrites(t, func(n int) error { return fmt.Errorf("rename %d refused", n) })
}

// A failure after the provider has answered leaves a spent token in a home with
// something alive in it. That is an error a person sees, naming the account, and
// the next look finishes the write with the token already on disk.
func TestAWriteThatFailsAfterARefreshIsSaidAndFinishedByTheNextLook(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, c := range []struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account) string
	}{
		{"a stranded home", stranded},
		{"a blanked home", cleared},
	} {
		for _, look := range everyLook() {
			t.Run(c.what+", finished by "+look.what, func(t *testing.T) {
				f.refreshes.Store(0)
				s := openTemp(t)
				a := livingClaude(s, "u1")
				home := c.make(t, s, a)
				alive(t, home, "sessions/1.json", os.Getpid())
				failed := failRenames(t)
				errs := s.Maintain(context.Background())
				if len(errs) == 0 || !strings.Contains(errs[0].Error(), a.String()) {
					t.Fatalf("an error naming the account: %v", errs)
				}
				if failed.Load() == 0 || a.Token.Refresh != "R-new" {
					t.Fatalf("the write was tried with the new token: %d %q", failed.Load(), a.Token.Refresh)
				}
				if !strings.Contains(allFiles(t, storeDir(s)), "R-new") {
					t.Fatal("the new token is on disk")
				}
				renameFile = os.Rename
				look.do(t, s, a)
				if f.refreshes.Load() != 1 {
					t.Fatalf("the write needs no other refresh: %d", f.refreshes.Load())
				}
				if r, _ := readLogin(t, home); r != "R-new" {
					t.Fatalf("the home holds it: %q", r)
				}
				if owed := a.Extra[seedOwedKey]; owed != "" {
					t.Fatalf("and nothing is owed: %q", owed)
				}
				s.Maintain(context.Background())
				if f.refreshes.Load() != 1 {
					t.Fatalf("and that was the end of it: %d", f.refreshes.Load())
				}
			})
		}
	}
}

// A refresh that keeps the refresh token leaves nothing in Staged to say the home
// is behind, so what is owed has to be remembered: the home's access token is the
// revoked one, and a second refresh for it would be one too many.
func TestAWriteOwedAfterARefreshThatKeptTheTokenIsNotForgotten(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-u1") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	failRenames(t)
	if errs := s.Maintain(context.Background()); len(errs) == 0 {
		t.Fatal("an error")
	}
	if a.Extra[seedOwedKey] == "" {
		t.Fatal("it is owed")
	}
	renameFile = os.Rename
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if !strings.Contains(readFile(filepath.Join(home, ".credentials.json")), "A-new") || f.refreshes.Load() != 1 {
		t.Fatalf("the home has it, with no other refresh: %d", f.refreshes.Load())
	}
}

// Something else filling the home pays the debt: the home holds the account's
// login, so nothing is written over it.
func TestAWriteNoLongerOwedIsNotMade(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	failRenames(t)
	s.Maintain(context.Background())
	renameFile = os.Rename
	body := storeLogin("A-new", "R-new", a.Token.ExpiresAt, "1999999999000")
	writeFile(t, filepath.Join(home, ".credentials.json"), body)
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if a.Extra[seedOwedKey] != "" || readFile(filepath.Join(home, ".credentials.json")) != body {
		t.Fatalf("%q", a.Extra[seedOwedKey])
	}
}

// Somebody else's login in a home with something running on it is never written
// over, owed or not.
func TestAWriteOwedWaitsWhileSomebodyElsesLoginRunsThere(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = refreshesTo("A-new", "R-new")
		f.profile = profileOf("A-other", "someone-else")
	})
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	failRenames(t)
	s.Maintain(context.Background())
	renameFile = os.Rename
	body := storeLogin("A-other", "R-other", later(8*time.Hour), "1888888888000")
	writeFile(t, filepath.Join(home, ".credentials.json"), body)
	s.Maintain(context.Background())
	if readFile(filepath.Join(home, ".credentials.json")) != body {
		t.Fatal("left alone while something runs there")
	}
	os.Remove(filepath.Join(home, "daemon.lock"))
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, home); r != "R-new" {
		t.Fatalf("and the account's goes in once nothing does: %q", r)
	}
}

// allFiles is every file of a store directory, for looking for a token on disk.
func allFiles(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			all.WriteString(readFile(filepath.Join(dir, e.Name())))
		}
	}
	return all.String()
}

// A home that cannot be read once the provider has answered is the same failure
// in another place, and used to report success.
func TestAHomeThatCannotBeReadAfterARefreshIsAnErrorToo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	svc := service(t, home)
	k.items[svc] = readFile(filepath.Join(home, ".credentials.json"))
	os.Remove(filepath.Join(home, ".credentials.json"))
	ob.watch(s, a, "R-new")
	ob.then = func() { k.findCode = 36 }
	errs := s.Maintain(context.Background())
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), a.String()) {
		t.Fatalf("an error naming the account: %v", errs)
	}
	k.findCode = 0
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if f.refreshes.Load() != 1 {
		t.Fatalf("no other refresh: %d", f.refreshes.Load())
	}
	if r, _ := readLogin(t, home); r != "R-new" {
		t.Fatalf("the home holds it: %q", r)
	}
}

// A quiet home's refresh owes its home the same write as a rescue's.
func TestAQuietHomesFailedWriteIsFinishedByAListingToo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	for _, c := range []struct {
		what string
		do   func(s *Store, a *rota.Account) []error
	}{
		{"maintenance", func(s *Store, a *rota.Account) []error { return s.Maintain(context.Background()) }},
		{"a listing", func(s *Store, a *rota.Account) []error { return s.Refresh(context.Background(), true, a) }},
	} {
		t.Run(c.what, func(t *testing.T) {
			f.refreshes.Store(0)
			s := openTemp(t)
			a := livingClaude(s, "u1")
			home := s.Home(a)
			expire(a)
			writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
			a.StagedWritten()
			failRenames(t)
			errs := c.do(s, a)
			if len(errs) == 0 || !strings.Contains(errors.Join(errs...).Error(), a.String()) {
				t.Fatalf("an error naming the account: %v", errs)
			}
			renameFile = os.Rename
			if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
				t.Fatal(errs)
			}
			if r, _ := readLogin(t, home); r != "R-new" || f.refreshes.Load() != 1 {
				t.Fatalf("%q %d", r, f.refreshes.Load())
			}
		})
	}
}

// On macOS the item is gone by the time the rename fails, so the home holds no
// login at all and nothing would ever write it again. The write is tried again
// while the lock is held, with what was going to be written, which is the only
// copy left of what the item held beside the login.
func TestARenameThatFailsAfterTheItemWentIsTriedAgainUnderTheLock(t *testing.T) {
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
	lock := filepath.Join(home, claudeRefreshLock)
	tries := onHomeWrites(t, func(n int) error {
		if _, err := os.Stat(lock); err != nil {
			t.Errorf("the lock is held: %v", err)
		}
		if n == 1 {
			return errors.New("refused")
		}
		return nil
	})
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("the item is gone")
	}
	body := readFile(filepath.Join(home, ".credentials.json"))
	if r, _ := readLogin(t, home); r != "R-new" || !strings.Contains(body, "mcp-secret") {
		t.Fatalf("the login, and what was beside it: %q %s", r, body)
	}
	if tries.Load() != 2 {
		t.Fatalf("tried again: %d", tries.Load())
	}
}

// And when it goes on failing the home holds nothing, the error says so, and the
// next look writes it: a home with no login must not fall into the hold for as
// long as something runs there.
func TestAHomeLeftEmptyByAFailedWriteIsWrittenByTheNextLook(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := stranded(t, s, a)
	a.Extra[routeKey] = routeStored
	svc := service(t, home)
	k.items[svc] = readFile(filepath.Join(home, ".credentials.json"))
	os.Remove(filepath.Join(home, ".credentials.json"))
	failRenames(t)
	if errs := s.Maintain(context.Background()); len(errs) == 0 || !strings.Contains(errs[0].Error(), a.String()) {
		t.Fatalf("an error naming the account: %v", errs)
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("the item went")
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("and the file never came: %v", err)
	}
	renameFile = os.Rename
	if _, err := launchEnv(t, s, a); err != nil {
		t.Fatal(err)
	}
	if r, _ := readLogin(t, home); r != "R-new" || f.refreshes.Load() != 1 {
		t.Fatalf("%q %d", r, f.refreshes.Load())
	}
}

/* ------------------------------------------------- a blanked item, a good file --- */

// Claude Code reads the item and never the file while an item exists, so a
// blanked item signs the home out whatever the file holds. It counts as older
// than any file that holds a login. A blanked file never wins.
func TestABlankedItemIsOlderThanAnyUsableFile(t *testing.T) {
	good := storeLogin("A", "R", later(time.Hour), "1999999999000")
	older := storeLogin("A", "R", later(-time.Hour), "1999999999000")
	for _, c := range []struct {
		what       string
		file, item string
		wins       bool
	}{
		{"a good file beside a blanked item", good, clearedLogin, true},
		{"an expired one too", older, clearedLogin, true},
		{"a blanked file beside a blanked item", clearedLogin, clearedLogin, false},
		{"a blanked file beside a login", clearedLogin, good, false},
		{"half a file", `{"claudeAiOauth":{"refreshToken":"R"`, clearedLogin, false},
		{"a file with no expiry", `{"claudeAiOauth":{"refreshToken":"R"}}`, clearedLogin, false},
		{"an item with no login", good, `{"mcpOAuth":{}}`, false},
		{"a newer file", good, older, true},
		{"an older file", older, good, false},
	} {
		if got := fileIsNewer([]byte(c.file), []byte(c.item)); got != c.wins {
			t.Fatalf("%s: %v", c.what, got)
		}
	}
}

// rota takes the file's login, and the blanked item goes so Claude Code reads
// the file, with what the item kept beside the login carried over: the item is
// what Claude Code has been reading, so it is the one whose secrets are current.
func TestABlankedItemBesideAGoodFileIsTakenOutOfTheWay(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, c := range []struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account, home string)
		do   func(t *testing.T, s *Store, a *rota.Account)
	}{
		{"a quiet home at a launch", func(*testing.T, *Store, *rota.Account, string) {},
			func(t *testing.T, s *Store, a *rota.Account) {
				if _, err := launchEnv(t, s, a); err != nil {
					t.Fatal(err)
				}
			}},
		{"a window open at maintenance", func(t *testing.T, s *Store, a *rota.Account, home string) {
			alive(t, home, "sessions/1.json", os.Getpid())
		}, func(t *testing.T, s *Store, a *rota.Account) {
			if errs := s.Maintain(context.Background()); len(errs) != 0 {
				t.Fatal(errs)
			}
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			k := fakeKeychain(t)
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
			s := openTemp(t)
			a := livingClaude(s, "u1")
			home := s.Home(a)
			svc := service(t, home)
			a.StagedWritten()
			k.items[svc] = clearedLogin
			writeFile(t, filepath.Join(home, ".credentials.json"), strings.Replace(
				storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"), "mcp-secret", "file-mcp", 1))
			c.make(t, s, a, home)
			c.do(t, s, a)
			if a.Dead || a.Token.Refresh != "R-u1" || f.refreshes.Load() != 0 {
				t.Fatalf("a live login reads as one: %v %q %d", a.Dead, a.Token.Refresh, f.refreshes.Load())
			}
			if _, ok := k.item(svc); ok {
				t.Fatal("the blanked item is gone")
			}
			body := readFile(filepath.Join(home, ".credentials.json"))
			if r, _ := readLogin(t, home); r != "R-u1" || !strings.Contains(body, "mcp-secret") {
				t.Fatalf("the file holds the login and what the item kept: %q %s", r, body)
			}
		})
	}
}

// A file older than the account's own login is a leftover, not a login: the
// store is as blanked as it looks, and the account's own token is presented.
func TestAnOldFileBesideABlankedItemDoesNotStopARescue(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	k := fakeKeychain(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	home := s.Home(a)
	svc := service(t, home)
	a.Token.ExpiresAt = later(-time.Hour)
	a.StagedWritten()
	k.items[svc] = clearedLogin
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-old", "R-old", later(-48*time.Hour), "1999999999000"))
	alive(t, home, "sessions/1.json", os.Getpid())
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatal(errs)
	}
	if a.Dead || a.Token.Refresh != "R-new" || f.refreshes.Load() != 1 {
		t.Fatalf("%v %q %d", a.Dead, a.Token.Refresh, f.refreshes.Load())
	}
	if _, ok := k.item(svc); ok {
		t.Fatal("the blanked item is gone")
	}
	body := readFile(filepath.Join(home, ".credentials.json"))
	if r, _ := readLogin(t, home); r != "R-new" || !strings.Contains(body, "mcp-secret") {
		t.Fatalf("%q %s", r, body)
	}
}
