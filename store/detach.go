package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
)

// refreshMargin is what RefreshTimeout allows beyond the HTTP client's own
// ceiling: the save and the write into the home that keep what a refresh
// returned.
const refreshMargin = 5 * time.Second

// RefreshTimeout is how long work the store has started goes on once whoever
// asked for it has gone (see Refresh): the HTTP client's ceiling —
// rota.HTTPClient's Timeout, 30 seconds unless an application set another —
// and a margin for the save and the write into the home that keep what a
// refresh returned. A server that is stopping waits this long for a sweep
// still under way.
func RefreshTimeout() time.Duration {
	ceiling := 30 * time.Second
	if c := rota.HTTPClient; c != nil && c.Timeout > 0 {
		ceiling = c.Timeout
	}
	return ceiling + refreshMargin
}

// refreshTimeout is RefreshTimeout, for a test that cannot wait half a minute.
var refreshTimeout = RefreshTimeout

// askerKey keeps, in a context detach made, the context of whoever asked.
type askerKey struct{}

// detach is the context a look's or a launch's work runs under: ctx's values
// without its cancellation, so that whoever asked going away — an HTTP client
// hanging up, a server stopping, a context cancelled — never stops a refresh
// the provider may already have answered. That answer is the one copy of the
// token that replaces the one presented. Lost, it leaves the account holding a
// spent token, which the next look presents again.
//
// Once ctx is done, what is under way has refreshTimeout to finish, and is
// cancelled after that; nothing new starts (stopped). done lets go of it all,
// once the work has returned. Until then the work counts as under way in this
// process, which is what Settle waits for. A context detach made already comes
// back as it is: the first caller is the one whose going away counts.
func detach(ctx context.Context) (work context.Context, done func()) {
	if _, ok := ctx.Value(askerKey{}).(context.Context); ok {
		return ctx, func() {}
	}
	inflight.add(1)
	work, cancel := context.WithCancelCause(context.WithValue(context.WithoutCancel(ctx), askerKey{}, ctx))
	d := refreshTimeout()
	stop := context.AfterFunc(ctx, func() {
		t := time.AfterFunc(d, func() {
			cancel(fmt.Errorf("still under way %s after whoever asked went away: %w", d, context.Cause(ctx)))
		})
		context.AfterFunc(work, func() { t.Stop() })
	})
	var once sync.Once
	return work, func() {
		once.Do(func() {
			stop()
			cancel(context.Canceled)
			inflight.add(-1)
		})
	}
}

// inflight counts the work detach has handed out in this process and done has
// not let go of yet.
var inflight underWay

// underWay is a count of work, and idle is closed when it falls to nothing and
// made anew when it rises from nothing.
type underWay struct {
	sync.Mutex
	n    int
	idle chan struct{}
}

func (f *underWay) add(d int) {
	f.Lock()
	defer f.Unlock()
	if f.n == 0 && d > 0 {
		f.idle = make(chan struct{})
	}
	f.n += d
	if f.n == 0 {
		close(f.idle)
	}
}

// Settle waits until no work the store detached from its caller is under way
// anywhere in this process — a refresh whoever asked for went away from
// included (see Refresh) — or until ctx is done, and then says so with ctx's
// error. A process about to exit calls it, so that no refresh is cut short
// between the provider's answer and its save: once the answer has come, the
// token it replaced is spent. Work that starts while Settle waits is waited
// for too. rota serve's Stop calls it.
func Settle(ctx context.Context) error {
	inflight.Lock()
	if inflight.n == 0 {
		inflight.Unlock()
		return nil
	}
	idle := inflight.idle
	inflight.Unlock()
	select {
	case <-idle:
		return Settle(ctx) // something may have started since
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// errLeft is what stopped says, with why whoever asked went away.
var errLeft = errors.New("whoever asked went away, so what was not under way yet is left to the next look")

// stopped is nil until whoever asked for the work ctx carries has gone, and
// then the error that says so (errLeft): nothing new starts after that, and
// the accounts a look had not come to are left to the next one. A context
// detach did not make is its own asker.
func stopped(ctx context.Context) error {
	asker, ok := ctx.Value(askerKey{}).(context.Context)
	if !ok {
		asker = ctx
	}
	if asker.Err() == nil {
		return nil
	}
	return leftError{context.Cause(asker)}
}

// leftError is errLeft with why whoever asked went away: a context cancelled,
// a deadline, a signal.
type leftError struct{ cause error }

func (e leftError) Error() string   { return errLeft.Error() + ": " + e.cause.Error() }
func (e leftError) Unwrap() []error { return []error{errLeft, e.cause} }

// Left reports whether err says only that whoever asked went away before the
// work it names was under way — what Refresh, Rescue, Maintain and a launch
// return for what they left to the next look. A caller that went away on
// purpose, a command line that was interrupted, passes over these and still
// shows the rest: a refresh that was answered but could not be saved, above
// all. An error that carries anything else beside it is not Left.
func Left(err error) bool {
	return errors.Is(err, errLeft) && onlyLeft(err)
}

// onlyLeft reports whether every error at the end of err's chain says
// whoever asked went away, and nothing else.
func onlyLeft(err error) bool {
	switch e := err.(type) {
	case leftError:
		return true
	case interface{ Unwrap() []error }:
		for _, inner := range e.Unwrap() {
			if !onlyLeft(inner) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyLeft(e.Unwrap())
	}
	return err == errLeft
}

// codexRepairs reports whether staging a codex account will refresh it first:
// lib's codex Plan does when the login is short of its id_token or access
// token and has a refresh token to get them with.
func codexRepairs(a *rota.Account) bool {
	return rota.Flavor(a.Provider) == "codex" && a.Token.Refresh != "" &&
		(a.Extra["id_token"] == "" || a.Token.Access == "")
}

// beforeRefresh is stopped at the last moment before a's token would be sent
// for a refresh, so that every refresh still waiting for its answer when
// whoever asked went away was sent before then and has the whole bound. nil
// when no refresh would be sent, the token not having expired, so a caller
// going away makes no error of a refresh nobody needed.
func beforeRefresh(ctx context.Context, a *rota.Account) error {
	if !a.Expired() {
		return nil
	}
	return stopped(ctx)
}

// onceLeft keeps the first error that says only that whoever asked went away
// (Left) and drops the others like it: however many places of one look find
// it, it is said once. One that carries anything else is always kept.
func onceLeft(errs []error) []error {
	var out []error
	said := false
	for _, err := range errs {
		if Left(err) {
			if said {
				continue
			}
			said = true
		}
		out = append(out, err)
	}
	return out
}
