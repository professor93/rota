//go:build unix

package owner

import (
	"os"
	"syscall"
)

// OpenFlags are the flags a file only its owner may use is opened with, on
// top of whatever the caller asks for.
//
// O_NOFOLLOW refuses a symbolic link where the file should be, so a link
// somebody else planted cannot point a write at a file of their choosing —
// a credential, say. O_NONBLOCK makes a FIFO planted there fail at once
// (ENXIO, when nobody is reading it) instead of blocking the open until
// somebody does; one that does have a reader opens, and is then refused by
// the caller's check that what it opened is a plain file.
const OpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK

// Blocking clears O_NONBLOCK on a file opened with OpenFlags, once the
// caller knows it is a plain file. A regular file ignores the flag anyway,
// and Go writes one through in full; clearing it makes that not a thing
// anybody has to know.
func Blocking(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var set error
	if err := rc.Control(func(fd uintptr) { set = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return set
}
