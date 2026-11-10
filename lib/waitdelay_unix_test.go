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
// The deadline passes once the helper says it is up and the CLI has done all
// it does, so the helper alone is holding the pipes when the deadline matters,
// however slowly the two started.
//
// A run that ends by itself before the helper is up had no helper holding
// anything, most likely because macOS killed a fake inside exec, as it now and
// then does (see fakecli's warm): that takes the helper, or the CLI that would
// have started one. It is not the case under test, so fakecli.Up starts the
// run again, as a fake's first run is. That the run did not end under a live
// helper is checked rather than assumed, since it is the very thing under
// test: a helper that Up finds up after such an end was alive all along, and
// fails the test.
func TestARunEndsWhenItsDeadlineDoesEvenIfAHelperKeepsThePipes(t *testing.T) {
	dir := t.TempDir()
	type outcome struct {
		res *Result
		err error
	}
	var (
		d        *testDeadline
		done     chan outcome
		ended    chan struct{}
		finished string
		tries    int
	)
	pid := fakecli.Up(t, func(ready string) <-chan struct{} {
		// Fakes of its own for each try, so nothing a try finds is an earlier
		// try's: a helper that comes up late writes only its own file.
		tries++
		tryDir := filepath.Join(dir, strconv.Itoa(tries))
		finished = filepath.Join(tryDir, "cli.finished")
		// The helper leaves the CLI's process group, so killing the group
		// does not reach it, and keeps the inherited stdout and stderr for
		// 30s. The CLI prints its answer and touches a file on its way out.
		helper := fakecli.Install(t, tryDir, "helper", fakecli.Spec{Sleep: "30s", KeepStdin: true, Ready: ready})
		bin := fakecli.Install(t, tryDir, "daemonising-cli", fakecli.Spec{Detach: helper, KeepStdin: true, Touch: finished,
			Stdout: []string{`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"ok","num_turns":1}`}})
		t.Cleanup(func() {
			if pid := readyPid(ready); pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		})
		given := &Command{Bin: bin, BaseEnv: []string{"PATH=/usr/bin:/bin"}}
		a := &Account{ID: 1, Provider: "claude"}
		a.Token.Access = "tok"

		d = newTestDeadline()
		done = make(chan outcome, 1)
		logEnd(t, done)
		ended = make(chan struct{})
		go func(d *testDeadline, done chan outcome, ended chan struct{}) {
			res, err := Run(d, a, "", given, Spec{Prompt: "p", flavorOverride: "claude"}, nil, io.Discard)
			done <- outcome{res, err}
			close(ended)
		}(d, done, ended)
		return ended
	})
	// The helper is up. A helper that runs at all held the pipes from the
	// moment it was started, so the run must not have ended, now or before the
	// CLI has done all it does.
	for limit := time.Now().Add(30 * time.Second); !touched(finished); {
		select {
		case <-ended:
			o := <-done
			t.Fatalf("the run ended by itself (%s) while its helper, pid %d, held the pipes", describe(o.res, o.err), pid)
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(limit) {
			d.pass()
			<-done
			t.Fatalf("the CLI never finished, and its helper, pid %d, is up", pid)
		}
	}
	select {
	case <-ended:
		o := <-done
		t.Fatalf("the run ended by itself (%s) while its helper, pid %d, held the pipes", describe(o.res, o.err), pid)
	default:
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
}

// touched reports whether a fake has created the file it was told to.
func touched(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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

// testDeadline is a context whose deadline passes when the test says so rather
// than at a time fixed in advance. To a run it is any other deadline: Done
// closes, and Err says the deadline was exceeded.
type testDeadline struct {
	context.Context
	done chan struct{}
}

func newTestDeadline() *testDeadline {
	return &testDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (d *testDeadline) Done() <-chan struct{} { return d.done }

func (d *testDeadline) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// pass makes the deadline pass now.
func (d *testDeadline) pass() { close(d.done) }
