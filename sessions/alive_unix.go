//go:build unix

package sessions

import (
	"os"
	"strings"
	"syscall"
)

// alive reports whether a process id still names a running process. Signal 0
// asks the kernel that question without sending anything: a process rota does
// not own answers with a permission error, which is still proof it is there.
func alive(pid int) bool {
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
