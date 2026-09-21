//go:build linux || darwin

package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// reader reads the master in the background, because a pseudo-terminal that
// nobody drains fills its buffer and stops the child. Everything read is
// kept, and waitFor looks through all of it: a terminal echoes, wraps and
// interleaves, so a test that insisted on the next line would be a test
// about timing.
type reader struct {
	mu   sync.Mutex
	seen strings.Builder
	done chan struct{}
}

func drain(t *testing.T, f *os.File) *reader {
	t.Helper()
	r := &reader{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				r.mu.Lock()
				r.seen.Write(buf[:n])
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

func (r *reader) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen.String()
}

// waitFor gives the child a second and a half to say something. A terminal
// is slower than a pipe and a loaded machine is slower still, so this is
// generous; nothing here waits for it in the passing case.
func (r *reader) waitFor(t *testing.T, want string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		if strings.Contains(r.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q never arrived; the terminal said:\n%s", want, r.text())
}

// start runs a shell on a terminal and cleans up after the test whatever it
// does: a pseudo-terminal left open is a file descriptor and a process left
// behind.
func start(t *testing.T, script string, cols, rows uint16) (*os.File, *exec.Cmd, *reader) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	master, err := Start(cmd, cols, rows)
	if err != nil {
		t.Fatalf("starting a terminal: %v", err)
	}
	r := drain(t, master)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		master.Close()
		<-r.done
	})
	return master, cmd, r
}

// The child is at a terminal, and it can say how big it is: stty reads the
// size from the device, not from the environment, so this is the kernel's
// answer rather than an echo of what was asked for.
func TestTheChildIsAtATerminalOfTheSizeItWasGiven(t *testing.T) {
	_, _, r := start(t, "stty size; echo done", 120, 40)
	r.waitFor(t, "40 120")
	r.waitFor(t, "done")
}

// Typing reaches the child and its answer comes back, which is the whole
// point of the pair.
func TestInputReachesTheChildAndOutputComesBack(t *testing.T) {
	master, _, r := start(t, "read x; echo got:$x", 80, 24)
	if _, err := master.Write([]byte("hello\n")); err != nil {
		t.Fatalf("writing to the terminal: %v", err)
	}
	r.waitFor(t, "got:hello")
}

// A resize is seen by the child: the kernel sends SIGWINCH, the shell runs
// the trap, and stty reads the new size off the same device.
func TestAResizeIsSeenByTheChild(t *testing.T) {
	master, _, r := start(t, "trap 'stty size' WINCH; stty size; while true; do sleep 0.05; done", 80, 24)
	r.waitFor(t, "24 80")
	if err := Resize(master, 100, 30); err != nil {
		t.Fatalf("resizing: %v", err)
	}
	r.waitFor(t, "30 100")
}

// The child's exit closes the master: nothing else holds the slave, so the
// reader is released rather than left waiting for a process that has gone.
func TestTheChildsExitClosesTheMaster(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "echo bye")
	master, err := Start(cmd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	r := drain(t, master)
	r.waitFor(t, "bye")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the master never reported the end of the terminal")
	}
	// Reading a master whose only slave has gone is an error rather than a
	// clean end on Linux, which is why the pump treats both as the end.
	if _, err := master.Read(make([]byte, 1)); err == nil {
		t.Fatal("reading a finished terminal must fail")
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "input/output error") {
		t.Logf("the end of a terminal reads as %v", err)
	}
}

// A size of nothing is still a size the kernel takes; what matters is that
// Resize reports rather than hides a failure, so the socket can answer.
func TestResizeOnAClosedTerminalFails(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	master, err := Start(cmd, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	master.Close()
	if err := Resize(master, 10, 10); err == nil {
		t.Fatal("resizing a closed terminal must fail")
	}
}
