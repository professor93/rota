//go:build unix

package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
)

// ^C while the command waits ends the wait with the status an interrupt
// gives, and leaves the login to be finished later.
func TestLoginOnATerminalLeavesTheLoginOpenOnAnInterrupt(t *testing.T) {
	home := seedTwoClaude(t)
	typing(t, "")
	duringWait(t, func(ctx context.Context) {
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
	})
	out, errOut, code := call(t, "login", "12")
	if code != 130 {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	id := loginIDFrom(t, out)
	if !strings.Contains(errOut, "rota login "+id+" <code>") || !strings.Contains(parked(t, home), id) {
		t.Fatalf("not left open: %q %s", errOut, parked(t, home))
	}
}
