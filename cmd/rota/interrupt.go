package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/professor93/rota/cmd/rota/internal/signals"
	"github.com/professor93/rota/store"
)

// interruptedDuring runs look with an interrupt taken, and, outside Windows,
// SIGTERM and SIGHUP — a terminal closed under it — and reports whether one
// came. The first to come cancels look's context. The store then finishes
// a refresh it has under way, the save and the write into the home included,
// and starts nothing else (see store.Refresh), where the signal's default
// would have ended the process between the provider's answer and the save,
// with the token it replaced already spent. A signal this process was started
// ignoring stays ignored. Once look returns the signals go back to what they
// were, before anything is handed to the CLI, and only then is it asked
// whether one came, so that none is taken and then lost (see signals.During).
func interruptedDuring(look func(ctx context.Context)) (cut bool) {
	var sigs []os.Signal
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if (sig == os.Interrupt || runtime.GOOS != "windows") && !signal.Ignored(sig) {
			sigs = append(sigs, sig)
		}
	}
	return signals.During(sigs, func(ctx context.Context) {
		if lookBegun != nil {
			lookBegun(ctx)
		}
		look(ctx)
	})
}

// lookBegun, when set, is told of each look's context: how a test waits for a
// signal to have been taken, rather than for a moment to have passed.
var lookBegun func(context.Context)

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
