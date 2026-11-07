package fakecli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	Maybe()
	os.Exit(m.Run())
}

// A fake asked to warm up exits at once and does nothing it was installed
// to do: Link runs it that way, and nothing may come of that run.
func TestAFakeAskedToWarmUpDoesNothing(t *testing.T) {
	dir := t.TempDir()
	touched := filepath.Join(dir, "touched")
	path := Install(t, dir, "claude", Spec{Stdout: []string{"played"}, Touch: touched, Exit: 3})
	out, err := exec.Command(path, warmArg).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("a warm-up exits 0 and says nothing: %v %q", err, out)
	}
	if _, err := os.Stat(touched); err == nil {
		t.Fatal("a warm-up played the fake")
	}
	// Run for real, it plays its part.
	out, _ = exec.Command(path).Output()
	if string(out) != "played\n" {
		t.Fatalf("the fake said %q", out)
	}
}

// WaitReady returns once the fake is up, with the pid of the fake itself.
func TestWaitReadyGivesTheFakesPid(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	path := Install(t, dir, "claude", Spec{Ready: ready, Sleep: "30s", KeepStdin: true})
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if pid := WaitReady(t, ready); pid != cmd.Process.Pid {
		t.Fatalf("WaitReady says %d, the fake is %d", pid, cmd.Process.Pid)
	}
}
