//go:build darwin

package proc

import (
	"errors"
	"net"
	"syscall"
)

// PeerPID is the process at the other end of a unix socket, as the kernel
// recorded it when the connection was made (LOCAL_PEERPID at SOL_LOCAL):
// nothing the peer says about itself can change it.
func PeerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		pid, serr = syscall.GetsockoptInt(int(fd), 0 /* SOL_LOCAL */, 0x002 /* LOCAL_PEERPID */)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	if pid <= 0 {
		return 0, errors.New("proc: the peer has no process")
	}
	return pid, nil
}
