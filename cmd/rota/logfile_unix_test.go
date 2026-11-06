//go:build unix

package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// beforeLong fails the test if f has not returned in a few seconds: what is
// being tested is that nothing waits on a pipe.
func beforeLong(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s is still waiting on a FIFO", what)
	}
}

func fifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
}

// TestLogFileRefusesAFIFO: a FIFO at the path would block the open until
// somebody read it — at the start, a server that never starts; after a
// rotation, every request that logs frozen behind the log's lock. It is
// refused at once, with a reader or without.
func TestLogFileRefusesAFIFO(t *testing.T) {
	// At the start, nobody reading: refused by the file and by serve.
	path := filepath.Join(t.TempDir(), "rota.log")
	fifo(t, path)
	beforeLong(t, "the open", func() {
		if _, err := openLogFile(path, io.Discard); err == nil || !strings.Contains(err.Error(), "not a plain file") {
			t.Errorf("a FIFO at the path: %v", err)
		}
	})
	addr := refusing(t)
	beforeLong(t, "serve", func() {
		if _, errb, code := call(t, "serve", addr, "--log-file", path); code == 0 || !strings.Contains(errb, "not a plain file") {
			t.Errorf("serve took a FIFO: exit %d, stderr %q", code, errb)
		}
	})

	// Somebody reading: it opens, and what it opened is not a plain file.
	reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	beforeLong(t, "the open with a reader", func() {
		if _, err := openLogFile(path, io.Discard); err == nil || !strings.Contains(err.Error(), "not a plain file") {
			t.Errorf("a FIFO with a reader: %v", err)
		}
	})
	reader.Close()

	// After a rotation: the reopen does not wait either, and the log goes
	// to stderr.
	path = filepath.Join(t.TempDir(), "rota.log")
	var stderr bytes.Buffer
	lf, err := openLogFile(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer lf.Close()
	fmt.Fprintln(lf, "before")
	lf.mu.Lock()
	lf.rotate()
	lf.mu.Unlock()
	fifo(t, path)
	beforeLong(t, "the reopen", func() { fmt.Fprintln(lf, "after") })
	if got := stderr.String(); !strings.Contains(got, "not a plain file") || !strings.Contains(got, "after\n") {
		t.Fatalf("stderr after a FIFO at the reopen:\n%s", got)
	}
}
