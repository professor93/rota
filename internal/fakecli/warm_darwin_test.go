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

// A first run that macOS kills on its way in is started again, and the fake
// it was for is installed as if nothing had happened. The test stands in for
// syspolicyd, which now and then SIGKILLs a new executable inside exec.
func TestAFirstRunKilledOnItsWayInIsStartedAgain(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	t.Setenv(warmKillEnv, "1:"+runs)
	path := Install(t, t.TempDir(), "claude", Spec{Stdout: []string{"played"}})
	if raw, _ := os.ReadFile(runs); len(raw) != 2 {
		t.Fatalf("the killed first run is started once more: %d runs", len(raw))
	}
	if out, _ := exec.Command(path).Output(); string(out) != "played\n" {
		t.Fatalf("the fake said %q", out)
	}
}

// A fake killed every time it starts fails its warm-up after a few tries, by
// name and by the kill, rather than being started for ever.
func TestAFirstRunKilledEveryTimeFailsAfterItsTries(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	t.Setenv(warmKillEnv, "99:"+runs)
	path := filepath.Join(t.TempDir(), "claude")
	err := Link(path)
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("a fake killed every time fails its warm-up, by name and kill: %v", err)
	}
	if raw, _ := os.ReadFile(runs); len(raw) != warmTries {
		t.Fatalf("it was started %d times, and the tries are %d", len(raw), warmTries)
	}
}
