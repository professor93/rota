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
	cred, err := peerCred(c)
	if err != nil {
		return 0, err
	}
	if cred.Pid <= 0 {
		return 0, errors.New("proc: the peer has no process")
	}
	return int(cred.Pid), nil
}

// PeerUID is the user the process at the other end of a unix socket runs
// as, from the same record (SO_PEERCRED): its effective uid when it
// connected.
func PeerUID(c *net.UnixConn) (int, error) {
	cred, err := peerCred(c)
	if err != nil {
		return -1, err
	}
	return int(cred.Uid), nil
}

func peerCred(c *net.UnixConn) (*syscall.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return nil, err
	}
	if serr != nil {
		return nil, serr
	}
	return cred, nil
}
