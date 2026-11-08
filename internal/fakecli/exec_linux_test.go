package fakecli

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// A fake with Exec becomes that program once its stdin has ended, keeping
// its pid, as an exec does.
func TestAFakeExecsIntoAnotherKeepingItsPid(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "after.pid")
	after := Install(t, dir, "after", Spec{Ready: ready, Sleep: "30s", KeepStdin: true})
	before := Install(t, dir, "before", Spec{Exec: after})
	cmd := exec.Command(before)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if pid := WaitReady(t, ready); pid != cmd.Process.Pid {
		t.Fatalf("the program it became has pid %d, the fake had %d", pid, cmd.Process.Pid)
	}
}
