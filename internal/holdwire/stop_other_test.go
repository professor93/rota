//go:build !linux && !darwin

package holdwire

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// Where held sessions do not run there is no holder to stop, and both ways
// of trying say so rather than reporting a process that is not the holder.
func TestStopHolderRefusesHere(t *testing.T) {
	m := &Meta{HolderPID: os.Getpid(), HolderStart: time.Now(), BootID: "boot"}
	if _, err := StopHolder(&Record{ID: "0123456789abcdef", Dir: t.TempDir(), Meta: m}, time.Second); !errors.Is(err, ErrPlatform) {
		t.Fatalf("StopHolder answers %v, want ErrPlatform", err)
	}
	if err := Signal(m.HolderPID, m.HolderStart, m.HolderStartTicks, m.BootID, syscall.SIGKILL); !errors.Is(err, ErrPlatform) {
		t.Fatalf("Signal answers %v, want ErrPlatform", err)
	}
}
