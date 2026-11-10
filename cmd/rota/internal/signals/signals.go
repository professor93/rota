// Package signals takes signals for as long as one call lasts and says
// whether any came, without losing one on the way to letting them go: what
// the program does while it prepares an account, where a signal's default
// would end it between the provider's answer and the save.
package signals

import (
	"context"
	"os"
	"os/signal"
)

// During runs fn with sigs taken, under a context the first of them cancels,
// and reports whether one came. Once fn returns they go back to what they
// were, and only then is the question asked: a signal that reached the
// process before that is counted, and one after it does what it always did,
// so none is taken and then lost. Asking any earlier would miss one still on
// its way, and on Linux a signal sent to a process may be handled after kill
// has returned, by another of its threads. With no sigs, fn runs under a
// context nothing cancels: Notify with none would take every signal.
func During(sigs []os.Signal, fn func(ctx context.Context)) (came bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if len(sigs) == 0 {
		fn(ctx)
		return false
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, sigs...)
	defer signal.Stop(ch) // should fn panic
	heard := make(chan struct{})
	var took bool
	go func() {
		defer close(heard)
		select {
		case <-ch:
			took = true
			cancel()
		case <-ctx.Done():
		}
	}()
	fn(ctx)
	// Once Stop has returned, a signal delivered to ch is in it, if the
	// goroutine has not taken it.
	signal.Stop(ch)
	cancel()
	<-heard
	return took || len(ch) > 0
}
