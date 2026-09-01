//go:build !unix

package rota

import "os/exec"

// setPgid is a no-op outside Unix; killGroup kills just the child.
func setPgid(*exec.Cmd) {}

func killGroup(c *exec.Cmd) {
	if c.Process != nil {
		_ = c.Process.Kill()
	}
}
