//go:build unix

package rota

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A named pipe given as a settings file used to be opened and read, which
// blocks until something writes to it: a caller could park the check for
// as long as they liked. The file is sized first, and a pipe is refused
// without being opened.
func TestAFIFOConfigFileIsRefusedWithoutBlocking(t *testing.T) {
	root := t.TempDir()
	fifo := filepath.Join(root, "settings.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- (Spec{Prompt: "p", Settings: json.RawMessage(`"` + fifo + `"`)}).Check("claude", &Limits{Roots: []string{root}})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("a pipe is not a settings file: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check blocked on the pipe instead of refusing it")
	}
}
