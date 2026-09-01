//go:build windows

package sessions

import (
	"os"
	"syscall"
	"unsafe"
)

// Windows has no flock; kernel32's LockFileEx keeps the same promise for
// the running-list file, through syscall alone so the module stays
// dependency-free.
var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
)

const lockfileExclusive = 0x00000002

// lockFile takes an exclusive lock, blocking until any other rota process
// releases it. Closing the file releases it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var ol syscall.Overlapped
	r, _, callErr := procLockFileEx.Call(f.Fd(), lockfileExclusive, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		_ = f.Close()
		return nil, callErr
	}
	return f, nil
}
