//go:build linux

package proc

import (
	"errors"
	"net"
	"syscall"
)

// PeerPID is the process at the other end of a unix socket, as the kernel
// recorded it when the connection was made (SO_PEERCRED): nothing the peer
// says about itself can change it.
func PeerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	if cred.Pid <= 0 {
		return 0, errors.New("proc: the peer has no process")
	}
	return int(cred.Pid), nil
}
