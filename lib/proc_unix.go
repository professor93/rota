//go:build unix

package rota

import (
	"os/exec"
	"syscall"
)

// setPgid puts the CLI in its own process group, so killing it takes the
// helpers it spawned with it.
func setPgid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
		_ = c.Process.Kill()
	}
}
