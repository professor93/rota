//go:build unix

// Package proc answers the one question about another process that more than
// one part of rota asks: is it still there. The session listing asks it of
// an editor's lock file and of a run rota recorded; the store asks it of a
// Claude Code daemon and of the sessions registered in an account's home,
// because whether anything is alive there decides whether rota may touch that
// home's login. One answer, so the two can never disagree about a pid.
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
