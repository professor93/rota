package store

import (
	"context"
	"encoding/json"
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

// Whoever asked going away part of the way through an account — here while
// the home is read, before anything is sent — stops the refresh at the last
// moment before it would be sent, so a refresh still waiting for its answer
// when the caller left was always sent before, and has the whole bound.
func TestARefreshNotYetSentWhenTheCallerLeavesIsNotSent(t *testing.T) {
	storedRouteHere(t)
	for _, look := range cancelLooks() {
		t.Run(look.what, func(t *testing.T) {
			w := newLineageWorld(t, true)
			w.clock.pass(9 * time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.k.onFind = func(string) { cancel() }
			errs := look.do(ctx, w)
			if n := len(w.p.entries()); n != 0 {
				w.p.dump(t)
				t.Fatalf("rota sent a refresh after whoever asked had gone: %d", n)
			}
			if !slices.ContainsFunc(errs, func(err error) bool { return errors.Is(err, context.Canceled) }) {
				t.Errorf("nothing said the caller had gone: %v", errs)
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

// Settle waits for every piece of work detach has handed out in this process,
// work that starts while it waits included, and gives up when its own ctx is
// done.
func TestSettleWaitsForTheWorkUnderWay(t *testing.T) {
	bounded, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := Settle(bounded); err != nil {
		t.Fatalf("nothing should be under way: %v", err)
	}
	_, first := detach(context.Background())
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Settle(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Settle did not wait for the work under way: %v", err)
	}
	settled := make(chan error, 1)
	go func() { settled <- Settle(bounded) }()
	_, second := detach(context.Background()) // started while Settle waits
	first()
	first() // done twice counts once
	select {
	case err := <-settled:
		t.Fatalf("Settle returned with work still under way: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	second()
	select {
	case err := <-settled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Settle did not return once the work was done")
	}
}

// Left is what a caller that went away on purpose passes over: an error that
// says only that, whatever the reason it went — a cancelled context, a
// signal. One carrying anything else beside it is shown.
func TestLeftIsOnlyTheCallerGoingAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	left := stopped(ctx)
	signalled := leftError{errors.New("interrupt signal received")}
	failed := errors.New("the store could not be saved")
	for _, c := range []struct {
		what string
		err  error
		want bool
	}{
		{"left", left, true},
		{"left by a signal", signalled, true},
		{"left, wrapped", fmt.Errorf("account 1: not launched: %w", left), true},
		{"two of them", errors.Join(left, signalled), true},
		{"left beside a failure", errors.Join(left, failed), false},
		{"a failure", failed, false},
		{"a bare cancellation", context.Canceled, false},
		{"nothing", nil, false},
	} {
		if got := Left(c.err); got != c.want {
			t.Errorf("%s: Left(%v) = %v", c.what, c.err, got)
		}
	}
	if got := onceLeft([]error{left, failed, signalled, errors.Join(left, failed)}); len(got) != 3 {
		t.Errorf("onceLeft keeps one Left and everything else: %v", got)
	}
}

// A codex login short of its id_token is refreshed by its own staging, inside
// a launch. Once whoever asked has gone, that refresh is not sent either.
func TestACodexRepairIsNotSentOnceTheCallerHasGone(t *testing.T) {
	e := codexRefreshes(t)
	s := openTemp(t)
	a := s.add("codex")
	a.Token = rota.Token{Access: codexJWT(time.Now().Add(time.Hour)), Refresh: "r-old",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	a.Extra = map[string]string{"account_id": "acct"} // no id_token
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, release, err := s.prepareAlone(ctx, a, false)
	if release != nil {
		release()
	}
	if got := e.grants(); len(got) != 0 {
		t.Fatalf("the staging's repair was sent after the caller had gone: %v", got)
	}
	if !Left(err) {
		t.Fatalf("the launch should say only that the caller had gone: %v", err)
	}
}

// Preparing hands the launch a context of its own, and the launch heeds it
// beside the caller's: a caller already gone starts nothing whatever the hook
// hands, and Prepare and PrepareIn make their launch inside the hook as Run
// does, so a hook that says the launch was cut short gets no command back.
func TestPreparingIsHeededBesideTheCallersContextByEveryLaunch(t *testing.T) {
	storedRouteHere(t)
	t.Run("a caller gone before a run's launch", func(t *testing.T) {
		w := newLineageWorld(t, false)
		w.clock.pass(9 * time.Hour)
		calls := 0
		w.s.Preparing = func(prepare func(context.Context)) bool {
			calls++
			prepare(context.Background())
			return false
		}
		gone, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := w.s.Run(gone, w.a, rota.Spec{Prompt: "hi"}, nil, nil)
		if calls != 1 || !Left(err) || len(w.p.entries()) != 0 || w.a.Token.Refresh != "R0" {
			w.p.dump(t)
			t.Fatalf("nothing launched, nothing sent: calls=%d err=%v holds %q", calls, err, w.a.Token.Refresh)
		}
	})
	for _, prep := range []struct {
		what string
		do   func(s *Store, a *rota.Account) (func(), error)
	}{
		{"Prepare", func(s *Store, a *rota.Account) (func(), error) {
			_, _, release, err := s.Prepare(context.Background(), a)
			return release, err
		}},
		{"PrepareIn", func(s *Store, a *rota.Account) (func(), error) {
			_, _, release, err := s.PrepareIn(context.Background(), a, t.TempDir())
			return release, err
		}},
	} {
		t.Run(prep.what+" cut short by the hook", func(t *testing.T) {
			w := newLineageWorld(t, false)
			w.clock.pass(9 * time.Hour)
			calls := 0
			w.s.Preparing = func(prepare func(context.Context)) bool {
				calls++
				cut, cancel := context.WithCancel(context.Background())
				cancel()
				prepare(cut)
				return true
			}
			release, err := prep.do(w.s, w.a)
			if calls != 1 || !Left(err) || release != nil || len(w.p.entries()) != 0 {
				w.p.dump(t)
				t.Fatalf("made inside the hook, cut short, nothing sent: calls=%d err=%v", calls, err)
			}
			if w.s.claimed(w.a) {
				t.Fatal("the claim of a launch cut short is let go")
			}
		})
		t.Run(prep.what+" inside the hook", func(t *testing.T) {
			onPath(t, "claude") // Prepare looks the CLI up; a fake, never whatever is installed here
			w := newLineageWorld(t, false)
			w.clock.pass(9 * time.Hour)
			calls := 0
			w.s.Preparing = func(prepare func(context.Context)) bool {
				calls++
				prepare(context.Background())
				return false
			}
			release, err := prep.do(w.s, w.a)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if calls != 1 || w.a.Token.Refresh != "R1" || len(w.p.rotaSpent()) != 0 {
				w.p.dump(t)
				t.Fatalf("made inside the hook: calls=%d holds %q", calls, w.a.Token.Refresh)
			}
		})
	}
}

// Preparing calls prepare once, before it returns. A second call makes no second
// launch, one made after it has returned makes none at all, and one still under
// way when it returns is waited for, so its claim is neither read half-made nor
// lost.
func TestPreparingMakesOneLaunchHoweverItCallsPrepare(t *testing.T) {
	storedRouteHere(t)
	t.Run("twice", func(t *testing.T) {
		onPath(t, "claude")
		w := newLineageWorld(t, false)
		w.clock.pass(9 * time.Hour)
		w.s.Preparing = func(prepare func(context.Context)) bool {
			prepare(context.Background())
			prepare(context.Background())
			return false
		}
		_, _, release, err := w.s.Prepare(context.Background(), w.a)
		if err != nil {
			t.Fatal(err)
		}
		release()
		if got := w.p.rotaPresented(); len(got) != 1 || w.a.Token.Refresh != "R1" || w.s.claimed(w.a) {
			t.Fatalf("one launch, its claim let go: %+v holds %q claimed=%v", got, w.a.Token.Refresh, w.s.claimed(w.a))
		}
	})
	t.Run("after it has returned", func(t *testing.T) {
		w := newLineageWorld(t, false)
		w.clock.pass(9 * time.Hour)
		var late func(context.Context)
		w.s.Preparing = func(prepare func(context.Context)) bool {
			late = prepare
			return false
		}
		_, _, release, err := w.s.Prepare(context.Background(), w.a)
		if err == nil || release != nil || !strings.Contains(err.Error(), "never made") {
			t.Fatalf("no launch was made, and that is said: %v", err)
		}
		late(context.Background())
		if got := w.p.rotaPresented(); len(got) != 0 || w.s.claimed(w.a) {
			t.Fatalf("a call after it returned makes no launch: %+v claimed=%v", got, w.s.claimed(w.a))
		}
	})
	t.Run("still under way when it returns", func(t *testing.T) {
		onPath(t, "claude")
		w := newLineageWorld(t, false)
		w.clock.pass(9 * time.Hour)
		entered, proceed := make(chan struct{}), make(chan struct{})
		w.p.onRota = func(string) {
			close(entered)
			<-proceed
		}
		w.s.Preparing = func(prepare func(context.Context)) bool {
			go prepare(context.Background())
			<-entered
			close(proceed) // the answer comes as this hook returns
			return false
		}
		_, _, release, err := w.s.Prepare(context.Background(), w.a)
		if err != nil || release == nil {
			t.Fatalf("the launch under way is waited for: %v", err)
		}
		release()
		if w.a.Token.Refresh != "R1" || w.s.claimed(w.a) {
			t.Fatalf("its refresh kept, its claim let go: holds %q claimed=%v", w.a.Token.Refresh, w.s.claimed(w.a))
		}
	})
}

// blankedForRescue leaves a home Claude Code blanked moments after a look found
// the account's login there, so a rescue will present rota's copy, with something
// alive beside it. The login is rotated into the keychain item first where there
// is one, since a blanked item in front of rota's own current file is a covered
// home and not a blanked one.
func blankedForRescue(t *testing.T, w *lineageWorld, keychain bool) string {
	t.Helper()
	w.cc.start()
	want := "R0"
	if keychain {
		w.clock.pass(7*time.Hour + 56*time.Minute)
		t.Logf("daemon: %s", w.cc.refresh("claude-code daemon"))
		want = "R1"
	}
	lineageLooks()[1].do(t, w)
	if w.a.Token.Refresh != want {
		t.Fatalf("setup: rota holds %q, want %q", w.a.Token.Refresh, want)
	}
	w.cc.write(clearedLogin)
	w.clock.pass(4 * time.Minute)
	return want
}

// A rescue is a refresh like any other: the caller going away while the provider
// answers loses nothing. The new token is kept, saved and written into the home,
// and the token it replaced is never presented again.
func TestARescueOutlivesWhoeverAskedForIt(t *testing.T) {
	for _, m := range lineageModes() {
		t.Run(m.name, func(t *testing.T) {
			w := newLineageWorld(t, m.keychain)
			had := blankedForRescue(t, w, m.keychain)
			next := "R" + itoa(int(had[1]-'0')+1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w.p.onRota = func(string) { cancel() }
			for _, err := range w.s.Rescue(ctx, w.a) {
				t.Logf("    %v", err)
			}
			w.p.onRota = nil
			if ctx.Err() == nil {
				t.Fatal("rota presented nothing, so nobody went away")
			}
			if got := savedRefresh(t, w.s, w.a.ID); w.a.Token.Refresh != next || refreshOf(w.cc.store()) != next || got != next {
				t.Fatalf("the answer is kept: rota %q, home %q, on disk %q, want %q", w.a.Token.Refresh, refreshOf(w.cc.store()), got, next)
			}
			if bad := w.p.rotaSpent(); len(bad) != 0 || w.a.Dead {
				t.Fatalf("spent=%+v dead=%v", bad, w.a.Dead)
			}
		})
	}
}

// A rescue asked for by a caller already gone presents nothing and says why, and
// so does one whose caller goes while the home is read under the lock: the
// account stays as it was, and the next look makes the rescue on the same terms.
func TestARescueWhoseCallerHasGoneSendsNothing(t *testing.T) {
	for _, when := range []string{"before the look", "under the lock"} {
		t.Run(when, func(t *testing.T) {
			w := newLineageWorld(t, true)
			had := blankedForRescue(t, w, true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if when == "before the look" {
				cancel()
			} else {
				w.k.onFind = func(string) {
					if _, err := os.Stat(lockPath(w)); err == nil {
						cancel()
					}
				}
			}
			errs := w.s.Rescue(ctx, w.a)
			w.k.onFind = nil
			if n := len(w.p.rotaPresented()); n != 0 {
				w.p.dump(t)
				t.Fatalf("rota presented %d tokens after the caller had gone", n)
			}
			if !slices.ContainsFunc(errs, func(err error) bool { return errors.Is(err, context.Canceled) }) {
				t.Fatalf("nothing said the caller had gone: %v", errs)
			}
			if w.a.Dead || w.a.Token.Refresh != had {
				t.Fatalf("the account changed: dead=%v %q", w.a.Dead, w.a.Token.Refresh)
			}
			lineageLooks()[1].do(t, w)
			if got := w.p.rotaPresented(); len(got) != 1 || got[0].rt != had || got[0].wasSpent {
				t.Fatalf("the next look makes the rescue: %+v", got)
			}
		})
	}
}

// Joined to something already gone, a context is done at once, with its cause,
// not a moment later: what is gated on it must not get in first.
func TestAContextJoinedToSomethingGoneIsDoneAtOnce(t *testing.T) {
	why := errors.New("the reason it went")
	gone, cancel := context.WithCancelCause(context.Background())
	cancel(why)
	for i := 0; i < 1000; i++ {
		ctx, stop := joinContexts(context.Background(), gone)
		err, cause := ctx.Err(), context.Cause(ctx)
		stop()
		if err == nil || !errors.Is(cause, why) {
			t.Fatalf("join %d: %v, cause %v", i, err, cause)
		}
	}
}
