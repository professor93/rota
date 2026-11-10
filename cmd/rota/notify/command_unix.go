//go:build unix

package notify

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd's program as the leader of a process group of its
// own, so that killGroup reaches whatever it starts too.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills cmd's whole process group: the program and every helper
// it started that is still in its group.
func killGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		// Nothing of the group is left; the leader may still be a zombie
		// for Wait to reap, which a kill of it would not change.
		return nil
	}
	return err
}
