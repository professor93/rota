//go:build linux || darwin

package holdwire

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// tryLock takes path's flock exclusively without waiting for whoever holds
// it. Locked's probe holds the lock shared for a moment, so a lock that is
// busy is tried a few more times over a few milliseconds before it is
// called held: a probe must never make a holder or a daemon give up.
func tryLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if (err != syscall.EWOULDBLOCK && err != syscall.EINTR) || try == 9 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = f.Close()
	if err == syscall.EWOULDBLOCK {
		return nil, ErrHeld
	}
	return nil, err
}

// unlock lets go of a lock tryLock took. The unlock is said before the
// close so the lock goes now, even if a child mid-exec still shares the
// descriptor for an instant.
func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// Locked says whether somebody holds path's lock, without taking it: the
// file is never created, and the shared probe is let go of at once. A lock
// file that is not there is not locked. One that cannot be looked at is
// taken as locked, because locked means alive and nothing is deleted on a
// guess that it is not.
func Locked(path string) bool {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return true
}
