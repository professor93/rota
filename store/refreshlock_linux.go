package store

import (
	"syscall"
	"time"
	"unsafe"
)

// futimes sets an open file's atime and mtime to t. utimensat with no path
// sets the times of the descriptor itself, as glibc's futimens does, where
// syscall.Futimes would go through /proc, which a container may not have.
func futimes(fd int, t time.Time) error {
	ts := [2]syscall.Timespec{syscall.NsecToTimespec(t.UnixNano()), syscall.NsecToTimespec(t.UnixNano())}
	_, _, errno := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(&ts[0])), 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
