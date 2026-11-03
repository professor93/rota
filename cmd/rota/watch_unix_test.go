//go:build linux || darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/store"
)

// lockHeld reports whether somebody holds an exclusive lock on path.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func within(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The watcher is a real process of its own, started before the handover hands
// the claim on: in a session of its own, not this process's child, holding
// nothing but its own lock. Once the CLI has gone, the account's claim is free
// although the watcher is still there; and SIGTERM ends it.
func TestTheWatcherHoldsNoClaimAndEndsOnSIGTERM(t *testing.T) {
	_, home := seedLiving(t, "")
	// The watcher is this test binary answering to rota's command line.
	t.Setenv("ROTA_AS_RUN", "1")
	orig := startWatcher
	startWatcher = watchInBackground
	t.Cleanup(func() { startWatcher = orig })
	origExec := execProcess
	t.Cleanup(func() { execProcess = origExec })
	var cli *exec.Cmd
	execProcess = func(string, []string, []string) error {
		// Started the way an exec keeps descriptors: whatever is not
		// close-on-exec, the claim included, goes with it.
		cli = exec.Command("/bin/sleep", "30")
		return cli.Start()
	}
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if cli == nil || cli.Process == nil {
		t.Fatal("the handover never happened")
	}
	t.Cleanup(func() { _ = cli.Process.Kill(); _ = cli.Wait() })

	lock := filepath.Join(home, ".rota-watch.lock")
	within(t, "the watcher holds its lock", 10*time.Second, func() bool { return lockHeld(t, lock) })
	raw, err := os.ReadFile(lock)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("the lock names its watcher: %q", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if sid, err := sessionOf(pid); err != nil || sid != pid {
		t.Fatalf("the watcher leads a session of its own: sid %d, %v", sid, err)
	}
	if pid == cli.Process.Pid {
		t.Fatal("the watcher is not the CLI")
	}

	_ = cli.Process.Kill()
	_ = cli.Wait()
	s, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	inUse := s.InUse(s.Find(1))
	s.Close()
	if inUse {
		t.Fatal("with the CLI gone nothing holds the account's claim, the watcher included")
	}
	if !lockHeld(t, lock) {
		t.Fatal("while the watcher is still there")
	}

	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	within(t, "SIGTERM ends the watcher", 10*time.Second, func() bool { return !lockHeld(t, lock) })
	log, _ := os.ReadFile(filepath.Join(os.Getenv("ROTA_HOME"), "watch.log"))
	if !strings.Contains(string(log), "watching") || !strings.Contains(string(log), "signal") {
		t.Fatalf("its log says what it did: %q", log)
	}
}
