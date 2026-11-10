package attach

import (
	"testing"
)

// TestDaemonKeyIsCtrlBackslash: the 1.4.0 daemon leaves a session on
// Ctrl-\ alone, so that is the key every attach reads, whether a daemon runs
// in the directory or not.
func TestDaemonKeyIsCtrlBackslash(t *testing.T) {
	for _, dir := range []string{t.TempDir(), "", "/nonexistent/rota/terminals"} {
		if got := DaemonKey(dir); got != 0x1c {
			t.Errorf("DaemonKey(%q) = %#x, want 0x1c", dir, got)
		}
	}
}
