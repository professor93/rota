//go:build windows

package store

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Windows has no flock, but kernel32's LockFileEx is the same promise —
// an exclusive lock another process cannot take until this one lets go —
// reached through syscall alone so the module stays dependency-free. The
// same one byte at offset zero is locked by every rota process, which is
// what makes the exclusion mutual.
var (
	kernel32       = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx = kernel32.NewProc("LockFileEx")
)

const (
	lockfileExclusive       = 0x00000002
	lockfileFailImmediately = 0x00000001
)

func lockRegion(f *os.File, flags uintptr) error {
	var ol syscall.Overlapped
	r, _, err := procLockFileEx.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&ol)))
	if r == 0 {
		return err
	}
	return nil
}

// lockFile takes an exclusive lock, blocking until any other rota process
// releases it. Closing the file releases it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockRegion(f, lockfileExclusive); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// tryLockFile takes the same lock without waiting. ok is false when someone
// else holds it — an answer, not a failure.
func tryLockFile(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := lockRegion(f, lockfileExclusive|lockfileFailImmediately); err != nil {
		_ = f.Close()
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == 33 /* ERROR_LOCK_VIOLATION */ {
			return nil, false, nil // held by someone else: an answer, not a failure
		}
		return nil, false, err
	}
	return f, true, nil
}

// keepAcrossExec does nothing on Windows: there is no execve handover — the
// vendor CLI runs as a child while this process stays alive holding the
// lock, so there is nothing for the lock to survive.
func keepAcrossExec(*os.File) error { return nil }
