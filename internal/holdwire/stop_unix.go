//go:build linux || darwin

package holdwire

import (
	"errors"
	"syscall"
	"time"
)

// Signal sends sig to pid while it is still the process recorded with that
// start time in that boot, and to nothing otherwise: ErrNotSameProcess. A
// process that went between the check and the signal has done what any
// signal here asks of it, and is not an error.
func Signal(pid int, start time.Time, boot string, sig syscall.Signal) error {
	if !SameProcess(pid, start, boot) {
		return ErrNotSameProcess
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
