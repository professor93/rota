package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A watcher keeps a claude account's login alive in its home for as long as
// something runs there, by making the look `rota list` starts with every few
// minutes: Rescue, for that one account. It exists because an interactive run
// leaves no rota behind — rota becomes the CLI — and a home stranded or blanked
// under a window open for days was otherwise rescued only when somebody
// happened to run a command that looked at it.
//
// It adds no rule of its own. Every look takes the store's lock, and the
// rescue takes Claude Code's refresh lock and keeps every refusal rule, so a
// watcher beside a server, a listing or another launch is one more look and
// nothing else. A healthy home is left to Claude Code, as every look leaves
// it.

// WatchEvery is how often a watcher looks at the home it keeps.
var WatchEvery = 5 * time.Minute

// watchLock is held by the one watcher of a home, for as long as it runs.
const watchLock = ".rota-watch.lock"

// Watchable reports whether a watcher has anything to keep for this account:
// a claude account whose login rota keeps in its home, alive, whose last
// launch there took the stored route. An account on a token in its
// environment, one on a key or a token alone, and every account on Windows
// have nothing a watcher could do anything about.
func (s *Store) Watchable(a *rota.Account) bool {
	return rota.SharedHome(a.Provider) && s.keepsLogin(a) && a.Extra[routeKey] == routeStored
}

// Watch keeps the login of account id alive while something runs in its home.
// open opens the store, once a look, and say is told what the watcher did and
// what went wrong.
//
// The first look finds the account's home and takes the home's watch lock;
// a home another watcher keeps already is left to it, and this one returns
// without a word. Every WatchEvery after that it opens the store and returns
// when the account is gone, no longer Watchable, given another home, or when
// nothing is alive in the home — no daemon, no live session, no run holding
// the account; otherwise it rescues it. A store that cannot be opened is said
// and tried again at the next look.
//
// ctx ends the watching between looks, and while one waits for the store's
// lock, but never in the middle of a look: one stopped between a refresh and
// its save would lose the login.
func Watch(ctx context.Context, open func() (*Store, error), id int, say func(string)) {
	w := &watcher{open: open, id: id, say: say}
	defer w.release()
	for w.look(ctx) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(WatchEvery):
		}
	}
}

type watcher struct {
	open func() (*Store, error)
	id   int
	say  func(string)
	home string
	lock *os.File
}

func (w *watcher) release() {
	if w.lock != nil {
		_ = w.lock.Close()
	}
}

// look is one look; false is the end of the watching.
func (w *watcher) look(ctx context.Context) bool {
	s, ok := w.openStore(ctx)
	if s == nil {
		return ok
	}
	defer s.Close()
	s.Warn = w.say
	a := s.Find(w.id)
	switch {
	case a == nil:
		w.said("account %d is gone", w.id)
		return false
	case !s.Watchable(a):
		w.said("%s no longer keeps its login in its home", a)
		return false
	}
	home := s.Home(a)
	if w.lock == nil {
		f, got, err := tryLockFile(filepath.Join(home, watchLock))
		switch {
		case err != nil:
			w.said("%s: %v", a, err)
			return false
		case !got:
			return false // another watcher keeps this home
		}
		_, _ = f.WriteString(strconv.Itoa(os.Getpid()) + "\n")
		w.lock, w.home = f, home
		w.said("%s: watching %s", a, home)
		return true
	}
	if !sameDir(home, w.home) {
		w.said("%s has another home now", a)
		return false
	}
	if len(claudeLiveThings(home)) == 0 && !s.claimed(a) {
		w.said("%s: nothing runs in its home any more", a)
		return false
	}
	// Uncancellable, as every look of rota's is: see Watch.
	for _, err := range s.Rescue(context.WithoutCancel(ctx), a) {
		w.said("%v", err)
	}
	return true
}

// openStore opens the store, or returns nil: ok is true when the open failed
// and the next look tries again, false when ctx ended while it waited.
func (w *watcher) openStore(ctx context.Context) (s *Store, ok bool) {
	type opened struct {
		s   *Store
		err error
	}
	got := make(chan opened, 1)
	go func() {
		s, err := w.open()
		got <- opened{s, err}
	}()
	select {
	case <-ctx.Done():
		// Nothing is in flight yet. Whatever the open comes back with is let go.
		go func() {
			if o := <-got; o.s != nil {
				o.s.Close()
			}
		}()
		return nil, false
	case o := <-got:
		if o.err != nil {
			w.said("the store could not be opened, so the next look tries again: %v", o.err)
			return nil, true
		}
		return o.s, true
	}
}

func (w *watcher) said(format string, args ...any) {
	if w.say != nil {
		w.say(fmt.Sprintf(format, args...))
	}
}
