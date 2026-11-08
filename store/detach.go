package store

import (
	"context"
	"errors"
	"fmt"
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
// once the work has returned. A context detach made already comes back as it
// is: the first caller is the one whose going away counts.
func detach(ctx context.Context) (work context.Context, done func()) {
	if _, ok := ctx.Value(askerKey{}).(context.Context); ok {
		return ctx, func() {}
	}
	work, cancel := context.WithCancelCause(context.WithValue(context.WithoutCancel(ctx), askerKey{}, ctx))
	d := refreshTimeout()
	stop := context.AfterFunc(ctx, func() {
		t := time.AfterFunc(d, func() {
			cancel(fmt.Errorf("still under way %s after whoever asked went away: %w", d, context.Cause(ctx)))
		})
		context.AfterFunc(work, func() { t.Stop() })
	})
	return work, func() {
		stop()
		cancel(context.Canceled)
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
	return fmt.Errorf("%w: %w", errLeft, context.Cause(asker))
}
