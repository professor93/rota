//go:build unix

// Package proc answers the questions about another process that more than
// one part of rota asks. The first is whether it is still there. The session
// listing asks it of an editor's lock file and of a run rota recorded; the
// store asks it of a Claude Code daemon and of the sessions registered in an
// account's home, because whether anything is alive there decides whether
// rota may touch that home's login. One answer, so the two can never disagree
// about a pid.
//
// The others are for held sessions, whose processes outlive the rota that
// started them: when a process started and which boot it runs in, so a pid
// recorded yesterday is never mistaken for whatever has that number now, and
// which process is at the other end of a unix socket.
package proc

import (
	"os"
	"strings"
	"syscall"
)

// Alive reports whether a process id still names a running process. Signal 0
// asks the kernel that question without sending anything: a process rota does
// not own answers with a permission error, which is still proof it is there.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || err == os.ErrPermission || strings.Contains(strings.ToLower(errText(err)), "permission")
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
