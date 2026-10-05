//go:build unix

package store

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func closeOnExec(t *testing.T, f *os.File) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}

// The handover replaces this process with the CLI, so the claim has to
// outlive the process image that took it — and only then. Unix-only twice
// over: the mechanism is fcntl, and the handover itself is execve.
func TestOnlyTheHandoverKeepsAClaimAcrossExec(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := keepAcrossExec(f); err != nil || closeOnExec(t, f) {
		t.Fatalf("the mechanism: after it, exec does not close the file (%v)", err)
	}

	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok"}}],"nextId":2}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	release, ok := st.holdRun(st.Find(1))
	if !ok {
		t.Fatal("nothing is holding it")
	}
	defer release()
	var held *os.File
	heldMu.Lock()
	for f := range heldClaims {
		held = f
	}
	heldMu.Unlock()
	if held == nil || !closeOnExec(t, held) {
		t.Fatal("a run's claim stays close-on-exec, so no child of this process holds it")
	}
	KeepClaimsAcrossExec()
	if closeOnExec(t, held) {
		t.Fatal("the handover arranges for the claim to outlive the process image that took it")
	}
}

// A child started while a run holds an account — another account's CLI, a
// shell on the server's terminal page — does not hold that account's claim
// once the run lets go of it.
func TestAChildStartedDuringARunDoesNotKeepTheAccountClaimed(t *testing.T) {
	s := openTemp(t)
	a := livingClaude(s, "u1")
	release, ok := s.holdRun(a)
	if !ok {
		t.Fatal("claim")
	}
	other := exec.Command("/bin/sleep", "5")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	if !s.claimed(a) {
		t.Fatal("held while the run lasts")
	}
	release()
	if s.claimed(a) {
		t.Fatal("the run is over, and a child that merely outlived it does not keep the account claimed")
	}
	if err := s.Removable(a); err != nil {
		t.Fatalf("so it can be removed: %v", err)
	}
}
