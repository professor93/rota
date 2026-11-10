//go:build linux || darwin

package hold

import (
	"syscall"
	"testing"
	"time"
)

// busy is the CPU time the process has had so far, all its threads
// together: what a test counts when it bounds how long its code takes, so
// that a machine too busy to give the test a CPU does not count against it.
// Waiting for a CPU is the machine's doing; only work is the code's.
func busy(t *testing.T) time.Duration {
	t.Helper()
	var u syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &u); err != nil {
		t.Fatal(err)
	}
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}
