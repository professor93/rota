//go:build unix

package store

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A pipe at the journal's path is never opened to wait on: the line is not
// written, and nothing waits.
func TestAPipeAtTheJournalsPathIsNotWaitedOn(t *testing.T) {
	s := openTemp(t)
	a := livingClaude(s, "u1")
	path := filepath.Join(storeDir(s), journalName)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		e := s.journalStart("adopt", a)
		e.finish("blanked")
		s.alreadySaid(e)
	}()
	select {
	case <-wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("a refresh waited on a pipe at the journal's path")
	}
}
