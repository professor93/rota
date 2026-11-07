package fakecli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A fake that cannot be started fails Link with a reason, at once, rather
// than leaving a test to find out later.
func TestAFakeThatCannotStartFailsItsLink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := warm(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a fake that cannot run fails its warm-up, by name: %v", err)
	}
}
