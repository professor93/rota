//go:build unix

package store

import (
	"os"
	"syscall"
	"testing"
)

// The handover replaces this process with the CLI, so the lock has to outlive
// the process image that took it. Go opens files close-on-exec, which would
// drop it at exactly the wrong moment. Unix-only twice over: the mechanism is
// fcntl, and the handover itself is execve — on Windows the CLI runs as a
// child while this process stays alive holding the lock, so there is nothing
// to survive.
func TestTheRunLockSurvivesTheHandover(t *testing.T) {
	// The mechanism: after this, exec does not close the file.
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := keepAcrossExec(f); err != nil {
		t.Fatal(err)
	}
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if flags&syscall.FD_CLOEXEC != 0 {
		t.Fatal("the lock would be dropped by exec, which is when it is most needed")
	}

	// And the wiring: a claim actually asks for it. Without this the
	// mechanism could be perfect and never called.
	kept := 0
	orig := keepingAcrossExec
	t.Cleanup(func() { keepingAcrossExec = orig })
	keepingAcrossExec = func(f *os.File) error {
		kept++
		return orig(f)
	}
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok"}}],"nextId":2}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	release, ok := st.holdForExec(st.Find(1))
	if !ok {
		t.Fatal("nothing is holding it")
	}
	release()
	if kept == 0 {
		t.Fatal("a claim must be arranged to outlive the process image that took it")
	}
}
