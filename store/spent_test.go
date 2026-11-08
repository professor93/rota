package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// reopen is the same store as another look of this process would open it,
// from what is on disk, after the first has let go of it.
func reopen(t *testing.T, s *Store, ob *orderBackend) (*Store, *rota.Account) {
	t.Helper()
	dir := ob.FileBackend.Dir
	s.Close()
	fb, err := NewFileBackend(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := NewStore(fb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { again.Close() })
	if len(again.Accounts) != 1 {
		t.Fatalf("one account: %d", len(again.Accounts))
	}
	return again, again.Accounts[0]
}

// A refusal is saved before anything else happens, and a save that fails is
// tried again: a refused token that reaches the disk as alive is presented
// again by whoever reads it next.
func TestARefusalIsSavedEvenWhenTheFirstSaveFails(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
	})
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	cleared(t, s, a)
	ob.failSaves = 1
	s.Rescue(context.Background(), a)
	if !ob.dead {
		t.Fatal("the refusal reached the disk")
	}
	again, b := reopen(t, s, ob)
	if !b.Dead {
		t.Fatal("and is what the next look reads")
	}
	again.Maintain(context.Background())
	if f.refreshes.Load() != 1 {
		t.Fatalf("presented once: %d", f.refreshes.Load())
	}
}

// When the disk takes nothing at all, the refusal is kept by the process, so no
// later look of it presents the token again, and it is saved the first time a
// save gets through.
func TestARefusalThatCouldNotBeSavedIsStillNeverPresentedAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) {
		f.refresh = func() (int, any) { return 400, map[string]string{"error": "invalid_grant"} }
	})
	s, ob := orderedStore(t)
	a := livingClaude(s, "u1")
	cleared(t, s, a)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	ob.failSaves = -1
	errs := s.Rescue(context.Background(), a)
	if len(errs) == 0 || ob.dead {
		t.Fatalf("a save that failed is said, and nothing was saved: %v", errs)
	}
	ob.failSaves = 0
	again, b := reopen(t, s, ob)
	if b.Dead {
		t.Fatal("the disk does not know yet")
	}
	again.Rescue(context.Background(), b)
	again.Maintain(context.Background())
	if f.refreshes.Load() != 1 {
		t.Fatalf("never presented again: %d", f.refreshes.Load())
	}
	if !b.Dead || !strings.Contains(b.WhyDead(), "invalid_grant") {
		t.Fatalf("dead, and still says why: %v %q", b.Dead, b.WhyDead())
	}
	if _, c := reopen(t, again, &orderBackend{FileBackend: again.Backend().(*FileBackend)}); !c.Dead {
		t.Fatal("and saved now")
	}
}

// A request that got no answer says nothing about the login, any more than an
// error the provider answered with: the account stays alive, says so, and the
// next look tries again, as every other refresh of rota's does. Dead would not
// stop a second presentation in a stranded home, whose daemon presents the same
// token every thirty seconds, and in a blanked one it would send the person to
// log in, which is what a rescue is for sparing them.
func TestARescueWhoseReplyWasLostIsTriedAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, c := range []struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account) string
	}{
		{"a blanked home", cleared},
		{"a stranded home", stranded},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) { f.refresh = func() (int, any) { return hangUp, nil } })
			s, ob := orderedStore(t)
			a := livingClaude(s, "u1")
			home := c.make(t, s, a)
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
			errs := s.Rescue(context.Background(), a)
			if f.refreshes.Load() != 1 || a.Dead || len(errs) != 1 || !strings.Contains(errs[0].Error(), a.String()) {
				t.Fatalf("presented once, alive, and said: %d %v %v", f.refreshes.Load(), a.Dead, errs)
			}
			if ob.dead {
				t.Fatal("nothing dead reached the disk")
			}
			f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
			again, b := reopen(t, s, ob)
			if errs := again.Rescue(context.Background(), b); len(errs) != 0 {
				t.Fatal(errs)
			}
			if r, _ := readLogin(t, home); r != "R-new" || f.refreshes.Load() != 2 || b.Dead {
				t.Fatalf("the next look gets through: %q %d %v", r, f.refreshes.Load(), b.Dead)
			}
		})
	}
}

// A provider that answered with an error says the token was not taken, so that
// is still tried again by the next look.
func TestAnAnsweredFailureIsStillTriedAgain(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = func() (int, any) { return 503, nil } })
	s := openTemp(t)
	a := livingClaude(s, "u1")
	cleared(t, s, a)
	s.Rescue(context.Background(), a)
	s.Rescue(context.Background(), a)
	if f.refreshes.Load() != 2 || a.Dead {
		t.Fatalf("%d %v", f.refreshes.Load(), a.Dead)
	}
}

/* --------------------------------------------- a keychain item written meanwhile --- */

// fresh is a login somebody made with /login while rota was writing.
var fresh = storeLogin("A-fresh", "R-fresh", later(8*time.Hour), "1777777777000")

func freshLogin() string { return fresh }

// duringWrite makes the keychain item change to a fresh login at the first read
// that comes while rota's new file is waiting, unrenamed, in the home: just before
// rota would delete the item.
func duringWrite(t *testing.T, k *keychain, home, svc string) {
	t.Helper()
	k.onFind = func(service string) {
		if service != svc {
			return
		}
		if tmp, _ := filepath.Glob(filepath.Join(home, ".credentials.json-*.tmp")); len(tmp) > 0 {
			k.items[svc] = freshLogin()
		}
	}
}

// /login does not take Claude Code's refresh lock, so a fresh login can land in
// the item between rota reading the home and deleting the item to write its
// own. The item is read again just before it is deleted, and left alone when
// it is not what rota judged: nothing is written, the error says so, and the
// next look finds the fresh login and takes it.
func TestAKeychainItemWrittenMeanwhileIsNotDeleted(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	t.Run("a rescue's write", func(t *testing.T) {
		k := fakeKeychain(t)
		f := fakeAnthropic(t)
		f.set(func(f *anthropic) {
			f.refresh = refreshesTo("A-new", "R-new")
			f.profile = profileOf("A-fresh", "u1")
		})
		s := openTemp(t)
		a := livingClaude(s, "u1")
		home := cleared(t, s, a)
		svc := service(t, home)
		k.items[svc] = readFile(filepath.Join(home, ".credentials.json"))
		os.Remove(filepath.Join(home, ".credentials.json"))
		alive(t, home, "sessions/1.json", os.Getpid())
		duringWrite(t, k, home, svc)
		errs := s.Rescue(context.Background(), a)
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), a.String()) {
			t.Fatalf("an error naming the account: %v", errs)
		}
		if v, _ := k.item(svc); v != freshLogin() {
			t.Fatalf("the fresh login stays: %q", v)
		}
		if _, err := os.Stat(filepath.Join(home, ".credentials.json")); !os.IsNotExist(err) {
			t.Fatalf("and nothing was written beside it: %v", err)
		}
		k.onFind = nil
		if errs := s.Rescue(context.Background(), a); len(errs) != 0 {
			t.Fatal(errs)
		}
		if a.Token.Refresh != "R-fresh" || a.Dead || a.Extra[seedOwedKey] != "" {
			t.Fatalf("the next look takes it: %q %v %q", a.Token.Refresh, a.Dead, a.Extra[seedOwedKey])
		}
		if v, _ := k.item(svc); v != freshLogin() || f.refreshes.Load() != 1 {
			t.Fatalf("and leaves it where it is: %d", f.refreshes.Load())
		}
	})
	t.Run("a blanked item taken out of the way", func(t *testing.T) {
		k := fakeKeychain(t)
		f := fakeAnthropic(t)
		f.set(func(f *anthropic) { f.profile = profileOf("A-fresh", "u1") })
		s := openTemp(t)
		a := livingClaude(s, "u1")
		home := s.Home(a)
		svc := service(t, home)
		a.StagedWritten()
		k.items[svc] = clearedLogin
		file := storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000")
		writeFile(t, filepath.Join(home, ".credentials.json"), file)
		alive(t, home, "sessions/1.json", os.Getpid())
		duringWrite(t, k, home, svc)
		errs := s.Rescue(context.Background(), a)
		if len(errs) == 0 || !strings.Contains(errs[0].Error(), a.String()) {
			t.Fatalf("an error naming the account: %v", errs)
		}
		if v, _ := k.item(svc); v != freshLogin() {
			t.Fatalf("the fresh login stays: %q", v)
		}
		if readFile(filepath.Join(home, ".credentials.json")) != file {
			t.Fatal("and the file is as it was")
		}
	})
}

// A launch or a listing that cannot save the store around a refresh of a login
// kept in its home never loses what it refreshed, so no refresh token is
// presented twice. Before it refreshes, it writes the login into the home if
// the home holds no usable login — an account signed in and never run since —
// and saves the store, and when that save fails it refreshes nothing. When the
// save after the refresh fails, the new login still goes into the home, which
// the store on disk says holds the login the refresh replaced, so the next look
// reads the new one back as a rotation.
//
// A home holding a usable login rota cannot place keeps it — one made inside
// Claude Code there is what the account falls back on should its own be
// refused — and nothing is written over it, before the refresh or after a
// save that failed. Such a refresh, alone, the next look cannot read back.
//
// A login held as a refresh token alone has nothing to write before the
// refresh; a home with no login at all is recorded as holding it, so what the
// refresh writes there after a failed save is read back all the same.
func TestARefreshTheStoreCannotSaveLosesNothing(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, look := range []struct {
		what string
		do   func(t *testing.T, s *Store, a *rota.Account) error
	}{
		{"a launch", func(t *testing.T, s *Store, a *rota.Account) error {
			_, err := launchEnv(t, s, a)
			return err
		}},
		{"a listing", func(t *testing.T, s *Store, a *rota.Account) error {
			return errors.Join(s.Refresh(context.Background(), true, a)...)
		}},
	} {
		for _, c := range []struct {
			what   string
			file   string // the refresh token the home holds: "" none
			staged string // what the store records of the home: "keep" its own login there
			kept   bool   // the home's login is one rota cannot place, and stays
		}{
			{"a home holding the login", "R-u1", "keep", false},
			{"a home holding the login, record unknown", "R-u1", "", false},
			{"signed in, never run, empty home", "", "-", false},
			{"signed in, never run, another login there", "R-prev", "-", true},
			{"held as a refresh token alone, signed in, never run, empty home", "", "alone", false},
			{"held as a refresh token alone, signed in, never run, another login there", "R-prev", "alone", true},
		} {
			// good is how many saves the look gets through before every one
			// fails: none, the one before the refresh, that and the one after.
			for _, good := range []int{0, 1, 2} {
				what := fmt.Sprintf("%s, %s, %d good saves", look.what, c.what, good)
				f := fakeAnthropic(t)
				f.set(func(f *anthropic) { f.refresh, f.singleUse = refreshesTo("A-new", "R-new"), true })
				m := &memBackend{home: t.TempDir()}
				left := -1
				s, err := NewStore(savesThenFails{Backend: m, left: &left})
				if err != nil {
					t.Fatal(err)
				}
				a := livingClaude(s, "u1")
				if c.file != "" {
					writeFile(t, filepath.Join(s.Home(a), ".credentials.json"),
						storeLogin("A-"+c.file, c.file, later(-time.Hour), "1999999999000"))
					a.StagedWritten()
				}
				switch c.staged {
				case "keep":
				case "alone":
					a.Token.Access = ""
					a.Staged = "-"
				default:
					a.Staged = c.staged
				}
				expire(a)
				if err := s.Save(); err != nil {
					t.Fatal(err)
				}

				left = good
				err = look.do(t, s, a)
				if err == nil {
					t.Fatalf("%s: a store that cannot be saved is said", what)
				}
				if got, want := f.grants(), []string{"R-u1"}[:min(good, 1)]; !slices.Equal(got, want) {
					t.Fatalf("%s: a look that cannot save before refreshing spends nothing, "+
						"and one that can spends the login once: presented %v", what, got)
				}
				s.Close() // the look's process ends
				left = -1
				if r, _ := readLogin(t, s.Home(a)); c.kept && good < 2 && r != c.file {
					t.Fatalf("%s: a login rota cannot place stays: the home holds %q", what, r)
				}
				if c.kept && good == 1 {
					if !strings.Contains(err.Error(), "held by this process alone") {
						t.Fatalf("%s: and the refreshed login is said to be nowhere else: %v", what, err)
					}
					continue
				}

				again, err := NewStore(m)
				if err != nil {
					t.Fatal(err)
				}
				b := again.Find(a.ID)
				if err := look.do(t, again, b); err != nil {
					t.Fatalf("%s: the next look: %v (presented %v)", what, err, f.grants())
				}
				again.Close()
				if got := f.grants(); !slices.Equal(got, []string{"R-u1"}) || b.Dead || b.Token.Refresh != "R-new" {
					t.Fatalf("%s: presented %v, dead=%v %q, token %q", what, got, b.Dead, b.DeadReason, b.Token.Refresh)
				}
				if r, _ := readLogin(t, again.Home(b)); r != "R-new" {
					t.Fatalf("%s: the home holds %q", what, r)
				}
			}
		}
	}
}

// A rescue whose every save fails still puts the login it refreshed into the
// home, where the next look reads it back, rather than nowhere: the login it
// replaced is spent, and presenting that one again is refused for good.
func TestARescueThatCannotSaveLosesNothing(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	for _, c := range []struct {
		what string
		make func(t *testing.T, s *Store, a *rota.Account) string
	}{
		{"a blanked home", cleared},
		{"a stranded home", stranded},
	} {
		f := fakeAnthropic(t)
		f.set(func(f *anthropic) { f.refresh, f.singleUse = refreshesTo("A-new", "R-new"), true })
		m := &memBackend{home: t.TempDir()}
		left := -1
		s, err := NewStore(savesThenFails{Backend: m, left: &left})
		if err != nil {
			t.Fatal(err)
		}
		a := livingClaude(s, "u1")
		home := c.make(t, s, a)
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}

		left = 0
		if errs := s.Rescue(context.Background(), a); len(errs) == 0 {
			t.Fatalf("%s: a store that cannot be saved is said", c.what)
		}
		s.Close()
		left = -1

		again, err := NewStore(m)
		if err != nil {
			t.Fatal(err)
		}
		b := again.Find(a.ID)
		if errs := again.Rescue(context.Background(), b); len(errs) != 0 {
			t.Fatalf("%s: the next look: %v (presented %v)", c.what, errs, f.grants())
		}
		again.Close()
		if got := f.grants(); !slices.Equal(got, []string{"R-u1"}) || b.Dead || b.Token.Refresh != "R-new" {
			t.Fatalf("%s: presented %v, dead=%v %q, token %q", c.what, got, b.Dead, b.DeadReason, b.Token.Refresh)
		}
		if r, _ := readLogin(t, home); r != "R-new" {
			t.Fatalf("%s: the home holds %q", c.what, r)
		}
	}
}

// A refreshed login the store could not save, and the home could not take
// either, is said to be where it is: in this process alone, the one it
// replaced spent. Nothing claims it was saved, or that a later look writes it.
func TestALoginNeitherSavedNorWrittenSaysSo(t *testing.T) {
	storedRouteHere(t)
	claudeWorld(t)
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh, f.singleUse = refreshesTo("A-new", "R-new"), true })
	var home string
	saves := 0
	b := hookedSaves{Backend: &memBackend{home: t.TempDir()}, before: func() error {
		saves++
		if saves <= 2 { // the store as the test leaves it, and the save before the refresh
			return nil
		}
		// The home cannot take the login: where its store goes, a directory.
		path := filepath.Join(home, ".credentials.json")
		os.Remove(path)
		writeFile(t, filepath.Join(path, "x"), "x")
		return errors.New("the disk is full")
	}}
	s, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := livingClaude(s, "u1")
	home = s.Home(a)
	writeFile(t, filepath.Join(home, ".credentials.json"), storeLogin("A-u1", "R-u1", later(-time.Hour), "1999999999000"))
	a.StagedWritten()
	expire(a)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	_, err = launchEnv(t, s, a)
	if err == nil || !slices.Equal(f.grants(), []string{"R-u1"}) {
		t.Fatalf("refreshed once, and refused: %v %v", err, f.grants())
	}
	msg := err.Error()
	if !strings.Contains(msg, "held by this process alone") || strings.Contains(msg, "refreshed and saved") ||
		strings.Contains(msg, "the next look writes it") {
		t.Fatalf("says where the login is, and promises nothing: %v", err)
	}
}

// hookedSaves is a backend that asks before each save, which fails with
// what before returns.
type hookedSaves struct {
	Backend
	before func() error
}

func (h hookedSaves) Save(b []byte) error {
	if err := h.before(); err != nil {
		return err
	}
	return h.Backend.Save(b)
}

// A listing that refreshes no login kept in a home saves as it always did:
// once when something changed, a refresh of a token rota alone holds
// included, and never when nothing did. Only a refresh of a login kept in a
// home is saved for first.
func TestAListingWithNoLoginInAHomeToRefreshSavesOnce(t *testing.T) {
	rota.Register(meteredRefresher{&fakeProvider{name: "t-listing-saves"}})
	m := &memBackend{home: t.TempDir()}
	s, err := NewStore(m)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Accounts = []*rota.Account{
		{ID: 1, Provider: "t-listing-saves", Order: 1, Token: rota.Token{Access: "good", Refresh: "r1", ExpiresAt: later(time.Hour)}},
		{ID: 2, Provider: "t-listing-saves", Order: 2, Token: rota.Token{Access: "old", Refresh: "r2", ExpiresAt: 1}},
	}
	if errs := s.Refresh(context.Background(), false); len(errs) != 0 {
		t.Fatal(errs)
	}
	if m.saves != 1 || s.Find(2).Token.Access != "new" || s.Find(1).Quota == nil {
		t.Fatalf("read, refreshed and saved once: saves=%d %+v", m.saves, s.Find(2).Token)
	}
	if errs := s.Refresh(context.Background(), false); len(errs) != 0 || m.saves != 1 {
		t.Fatalf("nothing due, nothing saved: %v saves=%d", errs, m.saves)
	}
}

// meteredRefresher is a provider with a usage endpoint and a refresh of its
// own, and no home.
type meteredRefresher struct{ *fakeProvider }

func (meteredRefresher) Quota(context.Context, string) (*rota.Quota, error) {
	return &rota.Quota{}, nil
}

func (meteredRefresher) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return &rota.Token{Access: "new", Refresh: "r3", ExpiresAt: later(time.Hour)}, nil
}
