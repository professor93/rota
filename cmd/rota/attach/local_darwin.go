//go:build darwin

package attach

import (
	"os"
	"syscall"
	"unsafe"
)

// The BSDs, macOS among them, call the two termios ioctls TIOCGETA and
// TIOCSETA.
const (
	localGetTermios    = syscall.TIOCGETA
	localSetTermiosNow = syscall.TIOCSETA
)

// localFlushInput drops what was typed and not yet read: tcflush(TCIFLUSH),
// which on the BSDs is TIOCFLUSH given a pointer to FREAD, the same 1 as
// TCIFLUSH.
func localFlushInput(f *os.File) error {
	which := int32(syscall.TCIFLUSH)
	return localIoctl(f, syscall.TIOCFLUSH, unsafe.Pointer(&which))
}
