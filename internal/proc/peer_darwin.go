//go:build darwin

package proc

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"unsafe"
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

// PeerUID is the user the process at the other end of a unix socket runs
// as, as the kernel recorded it when the connection was made
// (LOCAL_PEERCRED): its effective uid then.
//
// The standard library reads only an int at a time from getsockopt, and
// LOCAL_PEERCRED answers a struct xucred, so the call is made here with the
// struct laid out as <sys/ucred.h> has it.
func PeerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred xucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		n := uint32(unsafe.Sizeof(cred))
		if _, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, 0 /* SOL_LOCAL */, 0x001, /* LOCAL_PEERCRED */
			uintptr(unsafe.Pointer(&cred)), uintptr(unsafe.Pointer(&n)), 0); errno != 0 {
			serr = errno
		}
	}); err != nil {
		return -1, err
	}
	if serr != nil {
		return -1, serr
	}
	if cred.version != 0 /* XUCRED_VERSION */ {
		return -1, fmt.Errorf("proc: a peer credential of version %d", cred.version)
	}
	return int(cred.uid), nil
}

// xucred is <sys/ucred.h>'s struct xucred: a version, the effective uid,
// and the groups, sixteen at most.
type xucred struct {
	version uint32
	uid     uint32
	ngroups int16
	groups  [16]uint32
}
