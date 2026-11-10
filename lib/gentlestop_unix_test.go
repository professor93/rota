//go:build unix

package rota

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// graced makes the kill come after d, for as long as the test runs.
func graced(t *testing.T, d time.Duration) {
	t.Helper()
	old := killGrace
	killGrace = d
	t.Cleanup(func() { killGrace = old })
}

// gone waits a moment for a process to have died. A killed orphan is reaped by
// whoever adopted it, a little later than it dies, and where nothing reaps, as
// under a test binary that is a container's first process, it stays a zombie.
func gone(pid int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
		if raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			if i := bytes.LastIndexByte(raw, ')'); i > 0 && i+2 < len(raw) && raw[i+2] == 'Z' {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// logEnd logs, once the test is over, what a start's run ended with, if its
// outcome was not taken: a start that fakecli.Up gave up on, or failed the
// test for, is otherwise gone with the error that says why it never ran.
func logEnd[O any](t testing.TB, done <-chan O) {
	t.Cleanup(func() {
		select {
		case o := <-done:
			t.Logf("a run ended with: %+v", o)
		default:
		}
	})
}

func fakeRun(ctx context.Context, bin string) (*Result, error) {
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	var out bytes.Buffer
	return Run(ctx, a, "", &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}},
		Spec{Prompt: "p", flavorOverride: "claude"}, nil, &out)
}

// Cancelling a run asks the CLI to stop before anything kills it. Claude Code
// rotates its refresh token, and a process killed between the provider
// accepting a refresh and the store saving it destroys the login for the whole
// home. One that shuts down when asked gets to, and the run is not held up by
// the grace it did not need.
func TestACancelledRunLetsItsCLIStopByItself(t *testing.T) {
	dir := t.TempDir()
	stopped := filepath.Join(dir, "stopped")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res *Result
		err error
	}
	var done chan outcome
	fakecli.Up(t, func(ready string) <-chan struct{} {
		bin := fakecli.Install(t, t.TempDir(), "polite-cli", fakecli.Spec{Term: "exit", TermTouch: stopped, Ready: ready, Sleep: "30s"})
		done = make(chan outcome, 1)
		logEnd(t, done)
		ended := make(chan struct{})
		go func(done chan outcome) {
			res, err := fakeRun(ctx, bin)
			done <- outcome{res, err}
			close(ended)
		}(done)
		return ended
	})
	start := time.Now()
	cancel()
	o := <-done
	if took := time.Since(start); took >= killGrace {
		t.Fatalf("a CLI that stops when asked waits for nothing: %v", took)
	}
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("a cancelled run says so: %v", o.err)
	}
	if _, err := os.Stat(stopped); err != nil {
		t.Fatalf("the CLI got to shut down: %v", err)
	}
	if o.res == nil || o.res.ExitCode != 0 {
		t.Fatalf("and exited by itself: %+v", o.res)
	}
}

// A CLI that finishes by itself is judged by what it exits with, as ever.
func TestARunNobodyStoppedKeepsItsCLIsExit(t *testing.T) {
	dir := t.TempDir()
	for _, code := range []int{0, 3} {
		bin := fakecli.Install(t, dir, "plain-cli-"+strconv.Itoa(code), fakecli.Spec{Exit: code})
		res, err := fakeRun(context.Background(), bin)
		if err != nil || res == nil || res.ExitCode != code || res.IsError != (code != 0) {
			t.Fatalf("exit %d: %v %+v", code, err, res)
		}
	}
}

// One that takes no notice is killed once the grace is over, and the run ends
// then rather than never.
func TestACancelledRunKillsACLIThatIgnoresTheAsking(t *testing.T) {
	graced(t, 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var done chan error
	pid := fakecli.Up(t, func(ready string) <-chan struct{} {
		bin := fakecli.Install(t, t.TempDir(), "deaf-cli", fakecli.Spec{Term: "ignore", Ready: ready, Sleep: "30s"})
		done = make(chan error, 1)
		logEnd(t, done)
		ended := make(chan struct{})
		go func(done chan error) {
			_, err := fakeRun(ctx, bin)
			done <- err
			close(ended)
		}(done)
		return ended
	})
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled run says so: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a CLI that ignores the asking must not hold the run")
	}
	if took := time.Since(start); took < killGrace {
		t.Fatalf("it had its grace first: %v", took)
	}
	if !gone(pid) {
		t.Fatal("and is dead")
	}
}

// The asking goes to the group, so a helper hears it too, and a helper that
// stays behind after the CLI has gone is killed when the grace is over: the
// run still leaves nothing running.
func TestAStopReachesTheHelpersInTheGroup(t *testing.T) {
	graced(t, 300*time.Millisecond)
	dir := t.TempDir()
	heard, deafReady := filepath.Join(dir, "heard"), filepath.Join(dir, "deaf-ready")
	politeReady := filepath.Join(dir, "polite-ready")
	polite := fakecli.Install(t, dir, "polite-helper", fakecli.Spec{Term: "exit", TermTouch: heard, Ready: politeReady, Sleep: "30s"})
	deaf := fakecli.Install(t, dir, "deaf-helper", fakecli.Spec{Term: "ignore", Ready: deafReady, Sleep: "30s", Spawn: polite})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Only the CLI is started again should macOS kill it on its way in, and
	// then it has started no helper either. The helpers are waited for.
	var done chan error
	fakecli.Up(t, func(ready string) <-chan struct{} {
		bin := fakecli.Install(t, t.TempDir(), "parent-cli", fakecli.Spec{Term: "exit", TermTouch: filepath.Join(dir, "parent"), Ready: ready, Sleep: "30s", Spawn: deaf})
		done = make(chan error, 1)
		logEnd(t, done)
		ended := make(chan struct{})
		go func(done chan error) {
			_, err := fakeRun(ctx, bin)
			done <- err
			close(ended)
		}(done)
		return ended
	})
	helper := fakecli.WaitReady(t, deafReady)
	fakecli.WaitReady(t, politeReady)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run ends")
	}
	if !gone(helper) {
		t.Fatal("a helper that ignored the asking is gone when the run is over")
	}
	if _, err := os.Stat(heard); err != nil {
		t.Fatalf("and one that heard it stopped by itself: %v", err)
	}
}

// A helper that goes a moment after the CLI costs that moment and no more: the
// group is looked at again and again rather than once, so a stop does not wait
// out a grace nothing needs. Under a container's first process, where this
// test also runs, nothing reaps that helper, and a group of only zombies has
// nothing left in it either.
func TestAStopEndsAsSoonAsTheGroupHasGone(t *testing.T) {
	graced(t, 2*time.Second)
	dir := t.TempDir()
	helperReady := filepath.Join(dir, "helper-ready")
	helper := fakecli.Install(t, dir, "slow-helper", fakecli.Spec{Term: "exit", TermDelay: "150ms",
		TermTouch: filepath.Join(dir, "helper"), Ready: helperReady, Sleep: "30s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Only the CLI is started again should macOS kill it on its way in, and
	// then it has started no helper either. The helper is waited for.
	var done chan error
	fakecli.Up(t, func(ready string) <-chan struct{} {
		bin := fakecli.Install(t, t.TempDir(), "parent-cli", fakecli.Spec{Term: "exit", TermTouch: filepath.Join(dir, "parent"),
			Ready: ready, Sleep: "30s", Spawn: helper})
		done = make(chan error, 1)
		logEnd(t, done)
		ended := make(chan struct{})
		go func(done chan error) {
			_, err := fakeRun(ctx, bin)
			done <- err
			close(ended)
		}(done)
		return ended
	})
	pid := fakecli.WaitReady(t, helperReady)
	start := time.Now()
	cancel()
	<-done
	if took := time.Since(start); took >= killGrace {
		t.Fatalf("nothing was left to wait for: %v", took)
	}
	if !gone(pid) {
		t.Fatal("and the helper had its moment and went")
	}
	if _, err := os.Stat(filepath.Join(dir, "helper")); err != nil {
		t.Fatalf("by itself: %v", err)
	}
}

// What /proc says about a process is read from after the last parenthesis,
// because a process names itself and the name may hold anything.
func TestAProcessIsReadFromTheLineProcKeepsForIt(t *testing.T) {
	for _, c := range []struct {
		line  string
		state byte
		group int
		ok    bool
	}{
		{"12 (sleep) S 1 12 12 0 -1", 'S', 12, true},
		{"12 (a b) Z 1 900 900 0 -1", 'Z', 900, true},
		{"12 (odd) R 7 7) Z 1 44 44 0", 'Z', 44, true},
		{"12 (cut", 0, 0, false},
		{"12 (cut) S 1", 0, 0, false},
		{"12 (nan) S 1 x y", 0, 0, false},
	} {
		state, group, ok := procStat([]byte(c.line))
		if ok != c.ok || state != c.state || group != c.group {
			t.Fatalf("%q: %c %d %v", c.line, state, group, ok)
		}
	}
}

// Stopping twice asks once and cannot deadlock Wait, and a stop that comes
// after the CLI was waited for signals nothing: that pid is no longer ours.
func TestAStopAsksOnceAndCannotHoldUpWait(t *testing.T) {
	graced(t, 200*time.Millisecond)
	var bin string
	var c *exec.Cmd
	var halt *stopper
	fakecli.Up(t, func(ready string) <-chan struct{} {
		bin = fakecli.Install(t, t.TempDir(), "deaf-cli", fakecli.Spec{Term: "ignore", Ready: ready, Sleep: "30s"})
		cmd := exec.CommandContext(context.Background(), bin)
		setPgid(cmd)
		halt = newStopper(cmd)
		// Its standard output says when the CLI has gone without anybody
		// waiting for it, so the stops below still come before Wait, as a
		// run's do: a stop must return while nobody has waited yet.
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = w
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w.Close()
		// A start given up on is not left running. For the last one Wait has
		// been called by then, and this does nothing.
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		c = cmd
		ended := make(chan struct{})
		go func() {
			_, _ = io.Copy(io.Discard, r)
			r.Close()
			close(ended)
		}()
		return ended
	})
	halt.stop()
	armed := halt.kill
	halt.stop()
	if halt.kill != armed {
		t.Fatal("one kill, however often it is asked for")
	}
	done := make(chan struct{})
	go func() {
		_ = c.Wait()
		halt.settle()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait must end")
	}
	halt.stop()
	if !c.ProcessState.Exited() && c.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("killed: %v", c.ProcessState)
	}

	quiet := newStopper(exec.CommandContext(context.Background(), bin))
	quiet.settle()
	quiet.stop() // never started, and already over: nothing to signal
}

// A run that outgrows a bound has its CLI stopped too, and a CLI that neither
// listens nor stops writing cannot pin it: what it still prints is drained
// until the kill.
func TestARunThatHitsABoundEndsEvenIfItsCLIIgnoresTheAsking(t *testing.T) {
	graced(t, 300*time.Millisecond)
	var done chan error
	pid := fakecli.Up(t, func(ready string) <-chan struct{} {
		a := &Account{ID: 1, Provider: "claude"}
		a.Token.Access = "tok"
		bin := fakecli.Install(t, t.TempDir(), "loud-cli", fakecli.Spec{Term: "ignore", Ready: ready,
			Stdout: []string{strings.Repeat("x", 4096), "{{sleep:30s}}"}})
		var out bytes.Buffer
		done = make(chan error, 1)
		logEnd(t, done)
		ended := make(chan struct{})
		go func(done chan error) {
			_, err := Run(context.Background(), a, "", &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}},
				Spec{Prompt: "p", flavorOverride: "claude"}, &Limits{MaxBufferedOutput: 1024}, &out)
			done <- err
			close(ended)
		}(done)
		return ended
	})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run ends")
	}
	if !gone(pid) {
		t.Fatal("and the CLI with it")
	}
}
