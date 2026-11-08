package sessions

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The real lsof, asked at once about this test, a process that has just
// exited and launchd: the test's own directory comes back, and the two lsof
// leaves out, which make it exit 1, take nothing from it.
func TestOneLsofFindsThisProcesssDirectory(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("no lsof here:", err)
	}
	done := exec.Command("/usr/bin/true")
	if err := done.Run(); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	got := processCwds([]int{done.Process.Pid, os.Getpid(), 1})
	if got[1].err != nil {
		t.Fatalf("this process's own directory: %v", got[1].err)
	}
	if resolved(t, got[1].dir) != resolved(t, wd) {
		t.Fatalf("lsof says %q, the test is in %q", got[1].dir, wd)
	}
	if got[0].err == nil {
		t.Fatalf("a process that has exited has no directory: %+v", got[0])
	}
	if os.Geteuid() != 0 && got[2].err == nil {
		t.Fatalf("launchd is root's and not readable: %+v", got[2])
	}
}

func resolved(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
