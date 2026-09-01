//go:build unix

package sessions

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock, blocking until any other rota
// process releases it. Closing the file releases it, and Go opens files
// close-on-exec, so a process that hands itself over to a vendor CLI lets go
// even though it never returns to do it.
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
