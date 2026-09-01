//go:build unix

package store

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock, blocking until any other rota
// process releases it. Go opens files close-on-exec, so handing the process
// over to a vendor CLI releases it even if Close was never called.
// keepAcrossExec clears the close-on-exec flag.
//
// Go opens every file close-on-exec, which is right for almost everything and
// exactly wrong for this one: the handover replaces rota with the vendor CLI,
// and the claim on the account has to be held by whatever is running, not by
// the process image that happened to take it. The kernel releases it when that
// process finally exits, however it exits.
func keepAcrossExec(f *os.File) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), uintptr(syscall.F_SETFD), 0); errno != 0 {
		return errno
	}
	return nil
}

// tryLockFile takes the same lock without waiting. ok is false when someone
// else holds it, which is an answer rather than a failure: the caller wants to
// know whether it may proceed, not to queue behind whoever is there.
func tryLockFile(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, false, nil
	}
	return f, true, nil
}

func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
