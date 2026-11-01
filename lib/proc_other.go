//go:build !unix

package rota

import "os/exec"

// setPgid is a no-op outside Unix.
func setPgid(*exec.Cmd) {}

// stopper kills just the child outside Unix, and exec's own answer to the
// context stays what it was.
type stopper struct{ c *exec.Cmd }

func newStopper(c *exec.Cmd) *stopper { return &stopper{c: c} }

func (s *stopper) stop() {
	if s.c.Process != nil {
		_ = s.c.Process.Kill()
	}
}

// asked is false because a kill here leaves its own exit to judge the run by,
// which is what it always was.
func (s *stopper) asked() bool { return false }

func (s *stopper) settle() {}
