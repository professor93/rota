//go:build linux

package hold

import "syscall"

// Linux names the ioctl that reads a terminal's settings TCGETS.
const readTermios = syscall.TCGETS

// dupOnto makes newfd another name for oldfd. dup3 is the one every Linux
// architecture has; dup2 is missing from some.
func dupOnto(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }

// sessionOf is the session this process belongs to. Linux's syscall package
// has no Getsid of its own.
func sessionOf() (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, 0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}
