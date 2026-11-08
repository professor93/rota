package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/professor93/rota/store"
)

// interrupted is the context a command's own look at its accounts runs under:
// cancelled by the first interrupt, or, outside Windows, SIGTERM or SIGHUP —
// a terminal closed under it. The store then finishes a refresh it has under
// way, the save and the write into the home included, and starts nothing
// else (see store.Refresh), where the signal's default would have ended the
// process between the provider's answer and the save, with the token it
// replaced already spent. A signal this process was started ignoring stays
// ignored. stop puts the signals back as they were, and is called before
// anything is handed to the CLI; interruptedDuring does both.
func interrupted() (ctx context.Context, stop context.CancelFunc) {
	var sigs []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if (sig == os.Interrupt || runtime.GOOS != "windows") && !signal.Ignored(sig) {
			sigs = append(sigs, sig)
		}
	}
	if len(sigs) == 0 {
		// NotifyContext with no signals would take every one.
		return context.WithCancel(context.Background())
	}
	return signal.NotifyContext(context.Background(), sigs...)
}

// interruptedDuring runs look under interrupted's context, and reports whether
// a signal cut it short. It asks before it lets go of the signals, since
// letting go cancels the context too.
func interruptedDuring(look func(ctx context.Context)) (cut bool) {
	ctx, stop := interrupted()
	defer stop()
	look(ctx)
	return ctx.Err() != nil
}

// errInterrupted ends a command whose look a signal cut short, once what was
// under way is saved: with the status a shell gives a command an interrupt
// ended.
const errInterrupted = exitCode(130)

// interrupted ends a command a signal cut short. It says what went wrong
// beside the interrupt first — a refresh that was answered but could not be
// saved, above all, whose new login this process alone then holds — and
// passes over what only says the look left the rest for next time.
func (c *cli) interrupted(errs ...error) error {
	for _, err := range errs {
		if err != nil && !store.Left(err) {
			warn(c.err, "%v", err)
		}
	}
	return errInterrupted
}
