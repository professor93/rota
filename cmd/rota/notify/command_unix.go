//go:build unix

package notify

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// ownGroup starts cmd's program as the leader of a process group of its
// own, so that killGroup reaches whatever it starts too.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup kills cmd's whole process group: the program and every helper
// it started that is still in its group. It is os.ErrProcessDone when the
// program had already ended: no member of the group is left (ESRCH), or on
// macOS only zombies are (EPERM); the leader may still be a zombie for Wait
// to reap, which a kill would not change. Linux signals a zombie, so there
// EPERM is a live group the daemon may not signal, and stays an error.
func killGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err == syscall.ESRCH || err == syscall.EPERM && runtime.GOOS == "darwin" {
		return os.ErrProcessDone
	}
	return err
}
