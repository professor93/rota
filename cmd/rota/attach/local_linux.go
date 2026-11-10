//go:build linux

package attach

import (
	"os"
	"syscall"
)

// Linux calls the two termios ioctls TCGETS and TCSETS.
const (
	localGetTermios    = syscall.TCGETS
	localSetTermiosNow = syscall.TCSETS
)

// localFlushInput drops what was typed and not yet read: tcflush(TCIFLUSH),
// which on linux is TCFLSH taking its argument by value.
func localFlushInput(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioerr error
	if err := conn.Control(func(fd uintptr) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCFLSH, syscall.TCIFLUSH); errno != 0 {
			ioerr = errno
		}
	}); err != nil {
		return err
	}
	return ioerr
}
