package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cancelLook is one of rota's looks that refreshes an expired login in a
// quiet home, handed the context whoever asked for it would hand it.
type cancelLook struct {
	what string
	do   func(ctx context.Context, w *lineageWorld) []error
}

func cancelLooks() []cancelLook {
	return []cancelLook{
		{"rota list", func(ctx context.Context, w *lineageWorld) []error {
			return w.s.Refresh(ctx, false, w.a)
		}},
		{"rota serve maintenance", func(ctx context.Context, w *lineageWorld) []error {
			return w.s.Maintain(ctx)
		}},
		{"rota run (launch)", func(ctx context.Context, w *lineageWorld) []error {
			_, release, err := w.s.prepare(ctx, w.a, true)
			if err != nil {
				return []error{err}
			}
			release()
			return nil
		}},
	}
}

// savedRefresh is the refresh token the store on disk holds for account id.
func savedRefresh(t *testing.T, s *Store, id int) string {
	t.Helper()
	var doc struct {
		Accounts []struct {
			ID    int `json:"id"`
			Token struct {
				Refresh string `json:"refreshToken"`
			} `json:"token"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(readFile(filepath.Join(storeDir(s), "accounts.json"))), &doc); err != nil {
		t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.ID == id {
			return a.Token.Refresh
		}
	}
	return ""
}

// A refresh is not cancelled by whoever asked for it. The provider has
// rotated the login by the time it answers, so a refresh given up on with its
// caller would lose the only copy of the new token and leave the account on
// the spent one, which the next look presents again and so revokes the family.
// Here the caller goes away while the provider is answering, from every look
// that refreshes: rota keeps the answer, saves it, writes it into the home,
// and the next look presents the new token, never the old one.
func TestARefreshOutlivesWhoeverAskedForIt(t *testing.T) {
	for _, m := range lineageModes() {
		for _, look := range cancelLooks() {
			t.Run(m.name+"/"+look.what, func(t *testing.T) {
				w := newLineageWorld(t, m.keychain)
				w.clock.pass(9 * time.Hour) // expired, and nothing runs in the home
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				w.p.onRota = func(string) { cancel() }
				for _, err := range look.do(ctx, w) {
					t.Logf("    %v", err)
				}
				w.p.onRota = nil
				if ctx.Err() == nil {
					t.Fatal("rota presented nothing, so nobody went away")
				}
				if w.a.Token.Refresh != "R1" {
					t.Errorf("rota holds %q, not the token the provider answered with", w.a.Token.Refresh)
				}
				if got := refreshOf(w.cc.store()); got != "R1" {
					t.Errorf("the home holds %q, not the refreshed login", got)
				}
				if got := savedRefresh(t, w.s, w.a.ID); got != "R1" {
					t.Errorf("the store on disk holds %q, not the refreshed login", got)
				}
				// The next look, once that token has run out too.
				w.clock.pass(9 * time.Hour)
				for _, err := range look.do(context.Background(), w) {
					t.Logf("    again: %v", err)
				}
				w.p.dump(t)
				if bad := w.p.rotaSpent(); len(bad) != 0 {
					t.Errorf("rota presented a spent token: %+v", bad)
				}
				if w.a.Token.Refresh != "R2" || w.a.Dead {
					t.Errorf("the next look did not refresh the new token: holds %q, dead=%v (%s)",
						w.a.Token.Refresh, w.a.Dead, w.a.WhyDead())
				}
			})
		}
	}
}

// Whoever has gone away starts nothing: a look asked for by a caller already
// gone presents no token, and says why.
func TestALookWhoseCallerHasGoneStartsNothing(t *testing.T) {
	for _, look := range cancelLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, false)
			w.clock.pass(9 * time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			errs := look.do(ctx, w)
			if n := len(w.p.entries()); n != 0 {
				w.p.dump(t)
				t.Fatalf("rota presented %d tokens for a caller already gone", n)
			}
			if len(errs) == 0 || !errors.Is(errs[0], context.Canceled) {
				t.Fatalf("no error said the caller had gone: %v", errs)
			}
			if look.what != "rota run (launch)" && !errors.Is(errs[0], errLeft) {
				t.Errorf("the error does not say what was left: %v", errs[0])
			}
			if len(errs) != 1 {
				t.Errorf("said more than once: %v", errs)
			}
			if w.a.Token.Refresh != "R0" || w.a.Dead {
				t.Errorf("the account changed: %q dead=%v", w.a.Token.Refresh, w.a.Dead)
			}
		})
	}
}

// What detach hands the work: never the caller's cancellation, a bound of its
// own that starts only once the caller has gone, and nothing armed after the
// work has returned.
func TestDetachedWorkHasItsOwnBoundOnceTheCallerHasGone(t *testing.T) {
	old := refreshTimeout
	refreshTimeout = func() time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() { refreshTimeout = old })

	t.Run("caller gone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		work, done := detach(ctx)
		defer done()
		if stopped(work) != nil {
			t.Fatal("stopped before the caller went")
		}
		cancel()
		if work.Err() != nil {
			t.Fatal("the work was cancelled with its caller")
		}
		if err := stopped(work); !errors.Is(err, errLeft) || !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped: %v", err)
		}
		select {
		case <-work.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the work outlived its own bound")
		}
		if cause := context.Cause(work); !errors.Is(cause, context.Canceled) || !strings.Contains(cause.Error(), "under way") {
			t.Fatalf("cause: %v", cause)
		}
	})

	t.Run("no deadline while the caller waits", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		work, done := detach(ctx)
		defer done()
		time.Sleep(150 * time.Millisecond)
		if work.Err() != nil {
			t.Fatalf("the bound ran while the caller still waited: %v", context.Cause(work))
		}
		if _, ok := work.Deadline(); ok {
			t.Fatal("the work has a deadline of its own while the caller waits")
		}
	})

	t.Run("done", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		work, done := detach(ctx)
		done()
		if work.Err() == nil {
			t.Fatal("done left the work running")
		}
		cancel() // after done: nothing is armed, nothing fires
		time.Sleep(100 * time.Millisecond)
		if cause := context.Cause(work); cause != context.Canceled {
			t.Fatalf("cause after done: %v", cause)
		}
	})

	t.Run("detached once", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		outer, done := detach(ctx)
		defer done()
		inner, innerDone := detach(outer)
		innerDone()
		if inner != outer || outer.Err() != nil {
			t.Fatal("a second detach replaced the first")
		}
		cancel()
		if stopped(inner) == nil {
			t.Fatal("the first caller's going away does not count")
		}
	})
}
