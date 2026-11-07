//go:build unix

package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	// This test's own claim, by its path: another test may have left one
	// behind — a launch it made panic on purpose — and a run of this test
	// before this one has kept that one across exec already.
	var held *os.File
	mine := filepath.Join(st.Home(st.Find(1)), runLock)
	heldMu.Lock()
	for f := range heldClaims {
		if f.Name() == mine {
			held = f
		}
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

// A launch that goes through hands its claim on, and the handover keeps it
// across the exec: only a claim abandoned on the way to the CLI is let go of.
func TestALaunchHandedOverKeepsItsClaimAcrossExec(t *testing.T) {
	for _, k := range launchKinds(t) {
		t.Run(k.what, func(t *testing.T) {
			s, _ := panickingStore(t)
			a := k.make(s)
			_, release, err := s.prepare(context.Background(), a, true)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			KeepClaimsAcrossExec()
			var held *os.File
			mine := filepath.Join(s.Home(a), runLock)
			heldMu.Lock()
			for f := range heldClaims {
				if f.Name() == mine {
					held = f
				}
			}
			heldMu.Unlock()
			if held == nil || !s.claimed(a) {
				t.Fatal("the launch still holds the account")
			}
			if closeOnExec(t, held) {
				t.Fatal("and the exec keeps it")
			}
		})
	}
}

// A handover may lend its claims to a process of its own rather than keep them
// across the exec. The file is lent as it is, still close-on-exec, and a
// process given it holds the claim through its own descriptor once this one
// has let go, until that process ends: a flock belongs to the open file, and
// is gone when the last descriptor of it is.
func TestAClaimLentToAnotherProcessIsHeldUntilThatProcessEnds(t *testing.T) {
	dir := t.TempDir()
	writeAccounts(t, dir, `{"accounts":[{"id":1,"provider":"t-owns-creds","token":{"accessToken":"tok"}}],"nextId":2}`)
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := st.Find(1)
	release, ok := st.holdRun(a)
	if !ok {
		t.Fatal("nothing is holding it")
	}
	defer release()
	mine := filepath.Join(st.Home(a), runLock)
	lent := func() *os.File {
		for _, f := range ClaimFiles() {
			if f.Name() == mine {
				return f
			}
		}
		return nil
	}
	f := lent()
	if f == nil {
		t.Fatal("ClaimFiles lends the run's claim")
	}
	if !closeOnExec(t, f) {
		t.Fatal("lent, not kept across exec: it stays close-on-exec")
	}
	holder := exec.Command("/bin/sleep", "30")
	holder.ExtraFiles = []*os.File{f}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	release()
	if lent() != nil {
		t.Fatal("a claim this process let go of is not lent any more")
	}
	if !st.claimed(a) {
		t.Fatal("the process it was lent to holds it after this one has let go")
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	// A killed process's lock can be let go of a moment after it has been
	// waited for (macOS, under load).
	eventually(t, "the claim is free once the process it was lent to has ended", func() bool { return !st.claimed(a) })
}
