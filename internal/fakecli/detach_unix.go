//go:build unix

package fakecli

import (
	"os/exec"
	"syscall"
)

// detach starts cmd in a process group of its own, out of reach of a signal
// sent to the group of the fake that starts it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }
