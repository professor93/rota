package fakecli

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A fake that never ran a line — macOS killed it inside exec — is started
// again, with a ready file of its own, and the pid Up gives is that of the
// fake that came up.
func TestUpStartsAgainAFakeThatNeverRan(t *testing.T) {
	shortGrace(t)
	killed := "1:" + filepath.Join(t.TempDir(), "kills")
	var readies []string
	var last *exec.Cmd
	pid, r := up(t, func(tb testing.TB, ready string) <-chan struct{} {
		readies = append(readies, ready)
		var ended <-chan struct{}
		last, ended = startFake(tb, Spec{Ready: ready, Sleep: "30s", KeepStdin: true, KilledRuns: killed})
		return ended
	})
	if r.fatal != "" {
		t.Fatalf("a fake that never ran is started again, not failed: %s", r.fatal)
	}
	// macOS may kill one more on its own, so at least two starts.
	if len(readies) < 2 || pid != last.Process.Pid {
		t.Fatalf("%d starts, Up gave pid %d, the last fake is %d", len(readies), pid, last.Process.Pid)
	}
	for i := 1; i < len(readies); i++ {
		if readies[i] == readies[i-1] {
			t.Fatalf("each start has a ready file of its own: %v", readies)
		}
	}
}

// A fake that ran and still never came up is not macOS: the code under test
// or the fake ended it, and starting it again would hide that.
func TestUpFailsAFakeThatRanButNeverCameUp(t *testing.T) {
	shortGrace(t)
	missing := filepath.Join(t.TempDir(), Exe("missing"))
	starts := 0
	_, r := up(t, func(tb testing.TB, ready string) <-chan struct{} {
		starts++
		// It runs, then fails to start a helper that is not there, which it
		// does before it says it is up.
		_, ended := startFake(tb, Spec{Ready: ready, Spawn: missing, KeepStdin: true})
		return ended
	})
	if starts != 1 || !strings.Contains(r.fatal, "ran but its run ended") {
		t.Fatalf("a fake that ran fails at once: %d starts, %q", starts, r.fatal)
	}
}

// Fakes that never run, start after start, fail the test after startTries.
func TestUpGivesUpOnAFakeThatNeverRuns(t *testing.T) {
	shortGrace(t)
	killed := "100:" + filepath.Join(t.TempDir(), "kills")
	starts := 0
	_, r := up(t, func(tb testing.TB, ready string) <-chan struct{} {
		starts++
		_, ended := startFake(tb, Spec{Ready: ready, Sleep: "30s", KeepStdin: true, KilledRuns: killed})
		return ended
	})
	if starts != startTries || !strings.Contains(r.fatal, "killed before it ran a line") {
		t.Fatalf("%d starts, %q", starts, r.fatal)
	}
}

// recorder stands in for a test, so a test can watch Up fail one.
type recorder struct {
	testing.TB
	fatal string
}

func (r *recorder) Helper() {}

func (r *recorder) Fatal(args ...any) {
	r.fatal = fmt.Sprint(args...)
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.fatal = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// up runs Up on a goroutine of its own, where a recorded failure can end it.
func up(t *testing.T, start func(tb testing.TB, ready string) <-chan struct{}) (int, *recorder) {
	r := &recorder{TB: t}
	var pid int
	done := make(chan struct{})
	go func() {
		defer close(done)
		pid = Up(r, func(ready string) <-chan struct{} { return start(r, ready) })
	}()
	<-done
	return pid, r
}

// startFake installs a fake and starts it as the code under test would,
// returning a channel closed once it has exited.
func startFake(tb testing.TB, spec Spec) (*exec.Cmd, <-chan struct{}) {
	path := Install(tb, tb.TempDir(), "claude", spec)
	cmd := exec.Command(path)
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	ended := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(ended)
	}()
	tb.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})
	return cmd, ended
}

// shortGrace keeps a test of a fake that never ran from waiting the full
// grace for it: these fakes are the run, so they are gone when it ends.
func shortGrace(t *testing.T) {
	old := upGrace
	upGrace = 200 * time.Millisecond
	t.Cleanup(func() { upGrace = old })
}
