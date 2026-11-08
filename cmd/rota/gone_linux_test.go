package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// Where the kernel has no pidfd to give — before Linux 5.3, under a seccomp
// filter that refuses it, on MIPS — exitOf looks at /proc instead, and
// answers the same.
func TestExitOfAnswersByLookingWithoutAPidfd(t *testing.T) {
	lookOnly = true
	t.Cleanup(func() { lookOnly = false })
	exitOfAnswers(t)
}

// What /proc shows, look by look: a state, or an error.
type looks struct {
	mu    sync.Mutex
	shown []string
	read  int
}

func (l *looks) stat(pid int) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.read++
	if len(l.shown) == 0 {
		return nil, fs.ErrNotExist
	}
	s := l.shown[0]
	if len(l.shown) > 1 {
		l.shown = l.shown[1:]
	}
	switch s {
	case "gone":
		return nil, fs.ErrNotExist
	case "EMFILE":
		return nil, syscall.EMFILE
	case "other":
		return []byte(fmt.Sprintf("%d (co dex)) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 999 0 0", pid)), nil
	}
	return []byte(fmt.Sprintf("%d (co dex)) %s 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0 0", pid, s)), nil
}

// Through /proc a zombie is believed only once it stays one: a thread other
// than the leader that execs shows the process as a zombie for a moment,
// and an exec is not an exit. Gone is believed at once, and so is another
// process with the pid; a read that fails is looked at again.
func TestExitOfTakesALookThatMayBeAnExecAgain(t *testing.T) {
	lookOnly = true
	restore := []func(){func() { lookOnly = false }}
	for _, v := range []*time.Duration{&lookEvery, &zombieEvery} {
		was := *v
		*v = time.Millisecond
		restore = append(restore, func() { *v = was })
	}
	was := readStat
	restore = append(restore, func() { readStat = was })
	t.Cleanup(func() {
		for _, r := range restore {
			r()
		}
	})
	for _, c := range []struct {
		what  string
		shown []string
		// reads is how many looks it took to answer, the one that found
		// the process registered among them.
		reads int
	}{
		{"an exec, and then an exit", []string{"S", "S", "Z", "Z", "S", "S", "gone"}, 7},
		{"an exec seen as X", []string{"S", "X", "R", "gone"}, 4},
		{"reads that fail", []string{"S", "EMFILE", "EMFILE", "S", "gone"}, 5},
		{"another process with the pid", []string{"S", "S", "other"}, 3},
		{"a zombie gone while it was looked at again", []string{"S", "Z", "Z", "gone"}, 4},
		// 50ms of zombie at a look a millisecond: about fifty more looks.
		{"a zombie nobody waits for", []string{"S", "Z"}, 40},
	} {
		t.Run(c.what, func(t *testing.T) {
			l := &looks{shown: c.shown}
			readStat = l.stat
			wait, err := exitOf(4242)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("never answered")
			}
			l.mu.Lock()
			defer l.mu.Unlock()
			if c.what == "a zombie nobody waits for" {
				if l.read < c.reads {
					t.Fatalf("answered after %d looks, before the zombie had stayed one", l.read)
				}
				return
			}
			if l.read != c.reads {
				t.Fatalf("answered after %d looks, want %d", l.read, c.reads)
			}
		})
	}
}

// The real thing: a process that execs from a thread other than its main one
// is watched through /proc, looked at as often as can be, and is not taken
// to have exited until it has.
func TestExitOfTakesNoExecForAnExit(t *testing.T) {
	lookOnly = true
	was := lookEvery
	lookEvery = 0
	t.Cleanup(func() { lookOnly, lookEvery = false, was })
	bin, tmp := t.TempDir(), t.TempDir()
	beforeUp, afterUp := filepath.Join(tmp, "before.pid"), filepath.Join(tmp, "after.pid")
	after := fakecli.Install(t, bin, "after", fakecli.Spec{Ready: afterUp, Sleep: "30s", KeepStdin: true})
	before := fakecli.Install(t, bin, "before", fakecli.Spec{Ready: beforeUp, Exec: after})
	for trial := range 40 {
		_ = os.Remove(beforeUp)
		_ = os.Remove(afterUp)
		cmd := exec.Command(before)
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		pid := fakecli.WaitReady(t, beforeUp)
		wait, err := exitOf(pid)
		if err != nil {
			t.Fatal(err)
		}
		gone := make(chan struct{})
		go func() { wait(); close(gone) }()
		_ = in.Close() // the fake reads its stdin to the end, then execs
		if got := fakecli.WaitReady(t, afterUp); got != pid {
			t.Fatalf("trial %d: the exec kept the pid: %d, then %d", trial, pid, got)
		}
		select {
		case <-gone:
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("trial %d: an exec was taken for an exit", trial)
		default:
		}
		_ = cmd.Process.Kill()
		err = cmd.Wait()
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatal(err)
		}
		select {
		case <-gone:
		case <-time.After(10 * time.Second):
			t.Fatalf("trial %d: the exit was never answered", trial)
		}
	}
}
