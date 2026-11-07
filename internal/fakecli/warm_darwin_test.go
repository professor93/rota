package fakecli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fake that cannot be run fails its warm-up with a reason, at once,
// rather than leaving a test to find out later.
func TestAFakeThatCannotRunFailsItsWarmUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := warm(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("a fake that cannot run fails its warm-up, by name: %v", err)
	}
}

// A first run that never ends fails the warm-up once the limit has passed,
// instead of holding Link, and the test that called it, for good. yes stands
// in for a fake macOS never lets start: it takes no notice of the argument
// it is given and runs until it is stopped.
func TestAWarmUpThatNeverEndsFailsAtItsLimit(t *testing.T) {
	yes, err := exec.LookPath("yes")
	if err != nil {
		t.Skip("needs yes, a program that never ends by itself")
	}
	old := warmLimit
	warmLimit = 200 * time.Millisecond
	t.Cleanup(func() { warmLimit = old })
	start := time.Now()
	err = warm(yes)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the warm-up took %v against a limit of %v", took, warmLimit)
	}
	if err == nil || !strings.Contains(err.Error(), yes) || !strings.Contains(err.Error(), "had not ended after 200ms") {
		t.Fatalf("a warm-up past its limit fails, by name and limit: %v", err)
	}
}
