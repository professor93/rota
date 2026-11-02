//go:build unix

package rota

import (
	"bytes"
	"context"
	"errors"
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

// readyPID waits for a fake to say it is up and returns its pid.
func readyPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s never came up", path)
	return 0
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
	ready, stopped := filepath.Join(dir, "ready"), filepath.Join(dir, "stopped")
	bin := fakecli.Install(t, dir, "polite-cli", fakecli.Spec{Term: "exit", TermTouch: stopped, Ready: ready, Sleep: "30s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := fakeRun(ctx, bin)
		done <- outcome{res, err}
	}()
	readyPID(t, ready)
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

// The only results that differ from 1.3.0's are a CLI's own doing: one that
// handles the SIGTERM it is now asked with, and exits 0, reports that exit,
// where 1.3.0 killed it and reported -1. With nothing else to tell it apart
// from a CLI that exited 0 by itself just before it was asked, a failed events
// writer then returns the writer's error, where 1.3.0 returned nil and IsError.
// Telling the two apart needs the process's state before the signal, which
// /proc gives on Linux and nothing in Go's syscall package gives on macOS.
func TestOnlyACLIThatHandlesTheAskingChangesItsResult(t *testing.T) {
	dir := t.TempDir()
	for i, c := range stopCases() {
		if c.what == "its events writer failed after it ended" {
			continue // it ends by itself, and is in the matrix as it is
		}
		c.fake.Term = "exit"
		c.exit, c.isError = 0, false
		if c.what == "its events writer failed" {
			c.err = errSink
		}
		t.Run(c.what, func(t *testing.T) {
			res, err := runStopCase(t, dir, i, c)
			if res == nil {
				t.Fatalf("a result: %v", err)
			}
			if c.err == nil && err != nil || c.err != nil && !errors.Is(err, c.err) {
				t.Fatalf("error %v, want %v", err, c.err)
			}
			if res.ExitCode != c.exit || res.IsError != c.isError || res.Truncated != c.truncated {
				t.Fatalf("exit=%d is_error=%v truncated=%v, want %d %v %v",
					res.ExitCode, res.IsError, res.Truncated, c.exit, c.isError, c.truncated)
			}
		})
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
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	bin := fakecli.Install(t, dir, "deaf-cli", fakecli.Spec{Term: "ignore", Ready: ready, Sleep: "30s"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fakeRun(ctx, bin)
		done <- err
	}()
	pid := readyPID(t, ready)
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
	heard, deafReady, ready := filepath.Join(dir, "heard"), filepath.Join(dir, "deaf-ready"), filepath.Join(dir, "ready")
	politeReady := filepath.Join(dir, "polite-ready")
	polite := fakecli.Install(t, dir, "polite-helper", fakecli.Spec{Term: "exit", TermTouch: heard, Ready: politeReady, Sleep: "30s"})
	deaf := fakecli.Install(t, dir, "deaf-helper", fakecli.Spec{Term: "ignore", Ready: deafReady, Sleep: "30s", Spawn: polite})
	bin := fakecli.Install(t, dir, "parent-cli", fakecli.Spec{Term: "exit", TermTouch: filepath.Join(dir, "parent"), Ready: ready, Sleep: "30s", Spawn: deaf})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fakeRun(ctx, bin)
		done <- err
	}()
	readyPID(t, ready)
	helper := readyPID(t, deafReady)
	readyPID(t, politeReady)
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
	helperReady, ready := filepath.Join(dir, "helper-ready"), filepath.Join(dir, "ready")
	helper := fakecli.Install(t, dir, "slow-helper", fakecli.Spec{Term: "exit", TermDelay: "150ms",
		TermTouch: filepath.Join(dir, "helper"), Ready: helperReady, Sleep: "30s"})
	bin := fakecli.Install(t, dir, "parent-cli", fakecli.Spec{Term: "exit", TermTouch: filepath.Join(dir, "parent"),
		Ready: ready, Sleep: "30s", Spawn: helper})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fakeRun(ctx, bin)
		done <- err
	}()
	readyPID(t, ready)
	pid := readyPID(t, helperReady)
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
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	bin := fakecli.Install(t, dir, "deaf-cli", fakecli.Spec{Term: "ignore", Ready: ready, Sleep: "30s"})
	c := exec.CommandContext(context.Background(), bin)
	setPgid(c)
	halt := newStopper(c)
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	readyPID(t, ready)
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
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	bin := fakecli.Install(t, dir, "loud-cli", fakecli.Spec{Term: "ignore", Ready: ready,
		Stdout: []string{strings.Repeat("x", 4096), "{{sleep:30s}}"}})
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), a, "", &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}},
			Spec{Prompt: "p", flavorOverride: "claude"}, &Limits{MaxBufferedOutput: 1024}, &out)
		done <- err
	}()
	pid := readyPID(t, ready)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run ends")
	}
	if !gone(pid) {
		t.Fatal("and the CLI with it")
	}
}
