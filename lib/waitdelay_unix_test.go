//go:build unix

package rota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// A CLI that daemonises a helper — detached into a process group of its own,
// still holding the CLI's stdout and stderr — leaves the pipes open after the
// CLI itself is gone. Reading to EOF then waits for the helper, which is not
// what the caller's timeout meant; the run must end a bounded time after
// the deadline.
//
// The deadline passes once the helper says it is up, so it is holding the
// pipes when the deadline matters, however slowly the two started. A run that
// ends by itself before then had no helper holding anything: macOS now and
// then kills a fake inside exec — when the name the kernel has for the test
// binary's inode is a link another test has already removed — and that takes
// the helper, or the CLI that would have started one. It is not the case
// under test, so the run is started again, as a fake's first run is.
func TestARunEndsWhenItsDeadlineDoesEvenIfAHelperKeepsThePipes(t *testing.T) {
	dir := t.TempDir()
	// The helper leaves the CLI's process group, so killing the group does
	// not reach it, and keeps the inherited stdout and stderr for 30s.
	helperReady := filepath.Join(dir, "helper.ready")
	helper := fakecli.Install(t, dir, "helper", fakecli.Spec{Sleep: "30s", KeepStdin: true, Ready: helperReady})
	bin := fakecli.Install(t, dir, "daemonising-cli", fakecli.Spec{Detach: helper, KeepStdin: true,
		Stdout: []string{`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1}`}})
	t.Cleanup(func() {
		if pid := readyPid(helperReady); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	given := &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}}
	type outcome struct {
		res *Result
		err error
	}
	const tries = 3
	for try := 1; ; try++ {
		d := newDeadline()
		done := make(chan outcome, 1)
		go func() {
			res, err := Run(d, a, "", given, Spec{Prompt: "p", flavorOverride: "claude"}, nil, io.Discard)
			done <- outcome{res, err}
		}()
		// Whichever comes first: the helper saying it is up, or the run
		// ending by itself, which it cannot do while a helper holds its
		// pipes.
		var pid int
		var ended *outcome
		for limit := time.Now().Add(30 * time.Second); pid == 0 && ended == nil; {
			select {
			case o := <-done:
				ended = &o
			case <-time.After(5 * time.Millisecond):
				if pid = readyPid(helperReady); pid == 0 && time.Now().After(limit) {
					d.pass()
					<-done
					t.Fatal("the helper never said it was up")
				}
			}
		}
		if ended != nil {
			if try < tries {
				t.Logf("try %d: the run ended by itself before its helper was up (%s), so no helper held its pipes; again", try, describe(ended.res, ended.err))
				continue
			}
			t.Fatalf("the run ended by itself before its helper was up, %d times; the last: %s", tries, describe(ended.res, ended.err))
		}

		d.pass()
		passed := time.Now()
		o := <-done
		if took := time.Since(passed); took > waitDelay+5*time.Second {
			t.Fatalf("the run outlived its deadline by %v: a helper holding the pipes must not pin it", took)
		}
		if !errors.Is(o.err, context.DeadlineExceeded) {
			t.Fatalf("a run that hit its deadline says so: %v", o.err)
		}
		// And the helper is still there, out of the group's reach: the run
		// ended without it, rather than because it went.
		if syscall.Kill(pid, 0) != nil {
			t.Fatal("the helper is gone: killing the CLI's group reached it")
		}
		return
	}
}

// readyPid is the pid a fake wrote to its Ready file, or 0 while it has not.
func readyPid(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// describe says how a run ended, for a test that has to explain one.
func describe(res *Result, err error) string {
	if res == nil {
		return fmt.Sprintf("err %v", err)
	}
	return fmt.Sprintf("exit %d, err %v, stderr %q", res.ExitCode, err, res.Stderr)
}

// deadline is a context whose deadline passes when the test says so rather
// than at a time fixed in advance. To a run it is any other deadline: Done
// closes, and Err says the deadline was exceeded.
type deadline struct {
	context.Context
	done chan struct{}
}

func newDeadline() *deadline {
	return &deadline{Context: context.Background(), done: make(chan struct{})}
}

func (d *deadline) Done() <-chan struct{} { return d.done }

func (d *deadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// pass makes the deadline pass now.
func (d *deadline) pass() { close(d.done) }
