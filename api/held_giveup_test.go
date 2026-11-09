//go:build linux || darwin

package api

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// heldPID waits for a fake holder to write its pid, which it does once its
// signals are as it was told to set them.
func heldPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder never started")
		}
	}
}

// starting asks for a held session on a holder that never answers, and gives up
// on it once it has started and set its signals. It returns the holder's pid.
func starting(t *testing.T, env ...string) int {
	t.Helper()
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	gaveUp := make(chan time.Time)
	p := withHeld(t, append(env, "FAKE_HOLDER_SLOW=1h", "FAKE_HOLDER_PIDFILE="+pidfile), func(o *Options) {
		o.HoldDeadline = func() <-chan time.Time { return gaveUp }
	})
	pids := make(chan int, 1)
	go func() {
		pid := heldPID(t, pidfile)
		pids <- pid
		close(gaveUp)
	}()
	if resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1, "cols": 80, "rows": 24}); resp.StatusCode == 201 {
		t.Fatalf("a holder that never answered was a session: %s", raw)
	}
	pid := <-pids
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

// A daemon that gives up on a holder tells it to stop and does not kill it: it
// may be waiting for a refresh's answer, and a SIGKILL would lose the new token.
func TestAHolderGivenUpOnIsToldToStopAndNotKilled(t *testing.T) {
	old := holdKillAfter
	holdKillAfter = func() time.Duration { return time.Hour }
	t.Cleanup(func() { holdKillAfter = old })
	noted := filepath.Join(t.TempDir(), "term")
	pid := starting(t, "FAKE_HOLDER_NOTE_TERM="+noted)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(noted); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the holder was never told to stop")
		}
	}
	if !alive(pid) {
		t.Fatal("the holder was killed")
	}
}

// One that goes on regardless is killed, once it has had all a refresh may take.
func TestAHolderThatWillNotStopIsKilledInTheEnd(t *testing.T) {
	old := holdKillAfter
	holdKillAfter = func() time.Duration { return 20 * time.Millisecond }
	t.Cleanup(func() { holdKillAfter = old })
	pid := starting(t, "FAKE_HOLDER_IGNORE_TERM=1")
	for deadline := time.Now().Add(10 * time.Second); !gone(pid); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the holder was left running")
		}
	}
}
