package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// fastWatch makes a watcher look every few milliseconds instead of every few
// minutes.
func fastWatch(t *testing.T) {
	t.Helper()
	old := WatchEvery
	WatchEvery = 20 * time.Millisecond
	t.Cleanup(func() { WatchEvery = old })
}

// watchedStore writes a store holding one claude account on its stored login,
// shaped by prep, and returns how a watcher opens it and the account's home.
func watchedStore(t *testing.T, prep func(t *testing.T, s *Store, a *rota.Account)) (open func() (*Store, error), home string) {
	t.Helper()
	storedRouteHere(t)
	claudeWorld(t)
	fastWatch(t)
	dir := filepath.Join(t.TempDir(), "home")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := livingClaude(s, "u1")
	a.Extra[routeKey] = routeStored
	home = s.Home(a)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if prep != nil {
		prep(t, s, a)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return func() (*Store, error) { return Open(dir) }, home
}

// healthy leaves the account's current login in its home, as a launch does.
func healthy(t *testing.T, s *Store, a *rota.Account) {
	t.Helper()
	writeFile(t, filepath.Join(s.Home(a), ".credentials.json"),
		storeLogin(a.Token.Access, a.Token.Refresh, a.Token.ExpiresAt, "1999999999000"))
	a.StagedWritten()
}

// aWatcher is one watcher running, and what it has said.
type aWatcher struct {
	done   chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	words  []string
}

func (w *aWatcher) said() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.words, "\n")
}

func startWatch(t *testing.T, open func() (*Store, error)) *aWatcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &aWatcher{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		Watch(ctx, open, 1, func(m string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.words = append(w.words, m)
		})
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	return w
}

// ended reports whether the watcher returned within the time given.
func (w *aWatcher) ended(within time.Duration) bool {
	select {
	case <-w.done:
		return true
	case <-time.After(within):
		return false
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// watchHeld reports whether a watcher holds the home's watch lock.
func watchHeld(t *testing.T, home string) bool {
	t.Helper()
	f, ok, err := tryLockFile(filepath.Join(home, watchLock))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		f.Close()
	}
	return !ok
}

func storedRefresh(t *testing.T, open func() (*Store, error)) *rota.Account {
	t.Helper()
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	return s.Find(1)
}

// An interactive run leaves no rota process behind, so a home stranded or
// blanked while its window stays open for days was rescued only when somebody
// happened to run `rota list`. The watcher a run leaves is that look, made
// every few minutes for as long as something runs there: one refresh, saved
// and written in, and nothing more once the home holds the login again.
func TestAWatcherRescuesAStrandedOrBlankedHomeWhileSomethingRunsThere(t *testing.T) {
	for _, c := range []struct {
		what  string
		prep  func(t *testing.T, s *Store, a *rota.Account)
		alive string
	}{
		{"stranded", func(t *testing.T, s *Store, a *rota.Account) { stranded(t, s, a) }, "daemon.lock"},
		{"blanked", func(t *testing.T, s *Store, a *rota.Account) {
			home := cleared(t, s, a)
			alive(t, home, "sessions/1.json", os.Getpid())
		}, "sessions/1.json"},
	} {
		t.Run(c.what, func(t *testing.T) {
			f := fakeAnthropic(t)
			f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
			open, home := watchedStore(t, c.prep)
			w := startWatch(t, open)
			eventually(t, "the home holds the new login", func() bool { r, _ := readLogin(t, home); return r == "R-new" })
			// Several more looks find nothing owed.
			time.Sleep(10 * WatchEvery)
			if err := os.Remove(filepath.Join(home, c.alive)); err != nil {
				t.Fatal(err)
			}
			if !w.ended(5 * time.Second) {
				t.Fatal("a watcher whose home has gone quiet goes too")
			}
			if n := f.refreshes.Load(); n != 1 {
				t.Fatalf("one refresh: %d", n)
			}
			if a := storedRefresh(t, open); a.Token.Refresh != "R-new" || a.Dead {
				t.Fatalf("saved: %+v dead=%v", a.Token, a.Dead)
			}
			if !strings.Contains(w.said(), "refreshed it") {
				t.Fatalf("and said so: %q", w.said())
			}
			if watchHeld(t, home) {
				t.Fatal("and its lock went with it")
			}
		})
	}
}

// A healthy home is Claude Code's: the watcher looks and changes nothing.
func TestAWatcherLeavesAHealthyHomeToClaudeCode(t *testing.T) {
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	open, home := watchedStore(t, func(t *testing.T, s *Store, a *rota.Account) {
		healthy(t, s, a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	})
	before := readFile(filepath.Join(home, ".credentials.json"))
	var opens atomic.Int32
	w := startWatch(t, func() (*Store, error) { opens.Add(1); return open() })
	eventually(t, "several looks", func() bool { return opens.Load() >= 5 })
	w.cancel()
	if !w.ended(5 * time.Second) {
		t.Fatal("a watcher stops when told to")
	}
	if f.refreshes.Load() != 0 || f.profiles.Load() != 0 || f.usages.Load() != 0 {
		t.Fatalf("asked nobody anything: %d %d %d", f.refreshes.Load(), f.profiles.Load(), f.usages.Load())
	}
	if readFile(filepath.Join(home, ".credentials.json")) != before {
		t.Fatal("and wrote nothing")
	}
}

// Nothing alive is nothing to watch over: no daemon, no window, and no run
// holding the account. A run holding its claim alone keeps the watcher there.
func TestAWatcherGoesWhenTheHomeGoesQuiet(t *testing.T) {
	t.Run("nothing at all", func(t *testing.T) {
		fakeAnthropic(t)
		open, home := watchedStore(t, healthy)
		w := startWatch(t, open)
		if !w.ended(5 * time.Second) {
			t.Fatal("it goes")
		}
		if !strings.Contains(w.said(), "nothing runs") || watchHeld(t, home) {
			t.Fatalf("and says why: %q", w.said())
		}
	})
	t.Run("a claim, then none", func(t *testing.T) {
		fakeAnthropic(t)
		open, home := watchedStore(t, healthy)
		s, err := open()
		if err != nil {
			t.Fatal(err)
		}
		release, ok := s.holdRun(s.Find(1))
		s.Close()
		if !ok {
			t.Fatal("the claim")
		}
		var opens atomic.Int32
		w := startWatch(t, func() (*Store, error) { opens.Add(1); return open() })
		eventually(t, "several looks", func() bool { return opens.Load() >= 5 })
		if w.ended(0) || !watchHeld(t, home) {
			t.Fatalf("a run holding the account is something running: %q", w.said())
		}
		release()
		if !w.ended(5 * time.Second) {
			t.Fatal("and when it lets go the watcher goes")
		}
	})
}

// An account that has gone, or no longer keeps its login in that home, is
// nothing of the watcher's: it goes, and it never even takes the lock for one
// it was started for by mistake.
func TestAWatcherGoesWithTheAccountOrItsStoredLogin(t *testing.T) {
	cases := []struct {
		what string
		prep func(t *testing.T, s *Store, a *rota.Account)
		then func(t *testing.T, open func() (*Store, error))
	}{
		{"on the environment route from the start", func(t *testing.T, s *Store, a *rota.Account) {
			healthy(t, s, a)
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
			a.Extra[routeKey] = routeEnv
		}, nil},
		{"dead from the start", func(t *testing.T, s *Store, a *rota.Account) {
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
			a.Dead, a.DeadReason = true, "invalid_grant"
		}, nil},
		{"removed meanwhile", func(t *testing.T, s *Store, a *rota.Account) {
			healthy(t, s, a)
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		}, func(t *testing.T, open func() (*Store, error)) {
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.Accounts = nil
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
		}},
		{"moved to the environment route meanwhile", func(t *testing.T, s *Store, a *rota.Account) {
			healthy(t, s, a)
			alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		}, func(t *testing.T, open func() (*Store, error)) {
			s, err := open()
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.Find(1).Extra[routeKey] = routeEnv
			if err := s.Save(); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			fakeAnthropic(t)
			open, home := watchedStore(t, c.prep)
			w := startWatch(t, open)
			if c.then != nil {
				eventually(t, "it watches", func() bool { return watchHeld(t, home) })
				c.then(t, open)
			}
			if !w.ended(5 * time.Second) {
				t.Fatal("it goes")
			}
			if c.then == nil {
				if _, err := os.Lstat(filepath.Join(home, watchLock)); err == nil {
					t.Fatal("a watcher with nothing to watch leaves nothing in the home")
				}
			}
			if watchHeld(t, home) {
				t.Fatal("holding nothing")
			}
		})
	}
}

// One watcher per home: a second exits at once, and says nothing.
func TestASecondWatcherOfOneHomeGoesAtOnce(t *testing.T) {
	fakeAnthropic(t)
	open, home := watchedStore(t, func(t *testing.T, s *Store, a *rota.Account) {
		healthy(t, s, a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	})
	first := startWatch(t, open)
	eventually(t, "the first watches", func() bool { return watchHeld(t, home) })
	second := startWatch(t, open)
	if !second.ended(5 * time.Second) {
		t.Fatal("the second goes")
	}
	if second.said() != "" {
		t.Fatalf("quietly: %q", second.said())
	}
	if first.ended(0) || !watchHeld(t, home) {
		t.Fatal("the first stays")
	}
}

// The lock names the watcher holding it, and only it: a longer pid left there
// by an earlier one leaves no tail behind.
func TestTheWatchLockNamesItsWatcherAlone(t *testing.T) {
	fakeAnthropic(t)
	open, home := watchedStore(t, func(t *testing.T, s *Store, a *rota.Account) {
		healthy(t, s, a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
		writeFile(t, filepath.Join(s.Home(a), watchLock), "123456789012345678901234567890\n")
	})
	startWatch(t, open)
	want := strconv.Itoa(os.Getpid()) + "\n"
	eventually(t, "the lock names this watcher alone", func() bool {
		return readFile(filepath.Join(home, watchLock)) == want
	})
}

// A store that cannot be opened is said and tried again at the next look; it
// is never the end of the watcher, and leaves nothing held.
func TestAWatcherTriesAgainWhenTheStoreCannotBeOpened(t *testing.T) {
	f := fakeAnthropic(t)
	f.set(func(f *anthropic) { f.refresh = refreshesTo("A-new", "R-new") })
	open, home := watchedStore(t, func(t *testing.T, s *Store, a *rota.Account) { stranded(t, s, a) })
	var fails atomic.Int32
	fails.Store(3)
	w := startWatch(t, func() (*Store, error) {
		if fails.Add(-1) >= 0 {
			return nil, errors.New("the disk went away")
		}
		return open()
	})
	eventually(t, "the home holds the new login", func() bool { r, _ := readLogin(t, home); return r == "R-new" })
	if w.ended(0) || !strings.Contains(w.said(), "the disk went away") {
		t.Fatalf("said, and still there: %q", w.said())
	}
}

// Told to stop while another process holds the store, the watcher does not
// wait for it: there is nothing in flight to finish.
func TestAWatcherStopsAtOnceEvenWhileTheStoreIsHeld(t *testing.T) {
	fakeAnthropic(t)
	open, home := watchedStore(t, func(t *testing.T, s *Store, a *rota.Account) {
		healthy(t, s, a)
		alive(t, s.Home(a), "sessions/1.json", os.Getpid())
	})
	w := startWatch(t, open)
	eventually(t, "it watches", func() bool { return watchHeld(t, home) })
	s, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	time.Sleep(5 * WatchEvery) // waiting for the store by now
	w.cancel()
	if !w.ended(2 * time.Second) {
		t.Fatal("it stops")
	}
}
