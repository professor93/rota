//go:build !linux && !darwin

package proc

import (
	"errors"
	"os"
	"testing"
)

// Where held sessions do not run, every question asked only for them is
// refused, in the one sentence that says so.
func TestHeldQuestionsAreRefusedHere(t *testing.T) {
	if _, err := StartTime(os.Getpid()); !errors.Is(err, ErrPlatform) {
		t.Errorf("StartTime: %v", err)
	}
	if _, err := Parent(os.Getpid()); !errors.Is(err, ErrPlatform) {
		t.Errorf("Parent: %v", err)
	}
	if _, err := BootID(); !errors.Is(err, ErrPlatform) {
		t.Errorf("BootID: %v", err)
	}
	if _, err := PeerPID(nil); !errors.Is(err, ErrPlatform) {
		t.Errorf("PeerPID: %v", err)
	}
}
