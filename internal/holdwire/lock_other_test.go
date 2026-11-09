//go:build !linux && !darwin

package holdwire

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Where held sessions do not run, no lock and no daemon claim is taken, and
// nothing is ever held: even a lock file left by another system is free.
func TestHeldSessionsAreRefusedHere(t *testing.T) {
	dir := t.TempDir()
	if release, err := Lock(dir); !errors.Is(err, ErrPlatform) || release != nil {
		t.Errorf("Lock: %v", err)
	}
	terminals := filepath.Join(dir, "terminals")
	if release, err := ClaimDaemon(terminals, "test"); !errors.Is(err, ErrPlatform) || release != nil {
		t.Errorf("ClaimDaemon: %v", err)
	}
	if err := os.WriteFile(LockPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if Locked(LockPath(dir)) {
		t.Error("a lock is held here")
	}
	if d, ok := RunningDaemon(terminals); ok {
		t.Errorf("a daemon runs here: %+v", d)
	}
}
