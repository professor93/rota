package store

import (
	"syscall"
	"time"
)

// futimes sets an open file's atime and mtime to t.
func futimes(fd int, t time.Time) error {
	tv := syscall.NsecToTimeval(t.UnixNano())
	return syscall.Futimes(fd, []syscall.Timeval{tv, tv})
}
