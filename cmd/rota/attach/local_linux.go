//go:build linux

package attach

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Linux calls the two termios ioctls TCGETS and TCSETS.
const (
	localGetTermios    = syscall.TCGETS
	localSetTermiosNow = syscall.TCSETS
)

// localFlushInput drops what was typed and not yet read: tcflush(TCIFLUSH),
// which on linux is TCFLSH taking its argument by value. The syscall package
// names TCFLSH on only some architectures, x/sys/unix on all of them.
func localFlushInput(f *os.File) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioerr error
	if err := conn.Control(func(fd uintptr) {
		ioerr = unix.IoctlSetInt(int(fd), unix.TCFLSH, unix.TCIFLUSH)
	}); err != nil {
		return err
	}
	return ioerr
}
