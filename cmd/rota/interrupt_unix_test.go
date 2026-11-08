//go:build unix

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// An interrupt, a SIGTERM or a closed terminal during a command's look at its
// accounts ends the look, which finishes what is under way, rather than the
// process, which would not; and once the look is over the signal is this
// process's default again.
func TestASignalDuringALookEndsTheLookNotTheProcess(t *testing.T) {
	if interruptedDuring(func(context.Context) {}) {
		t.Fatal("a look nothing interrupted reads as interrupted")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if signal.Ignored(sig) {
			continue // left as this process was started, by design
		}
		cut := interruptedDuring(func(ctx context.Context) {
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
				t.Errorf("%v did not end the look", sig)
			}
		})
		if !cut {
			t.Errorf("%v: the look does not read as interrupted", sig)
		}
	}
}
