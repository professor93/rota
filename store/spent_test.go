package store

import (
	"context"
	"os"
	"path/filepath"
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
