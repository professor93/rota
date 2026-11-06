//go:build linux || darwin

package holdwire

import (
	"errors"
	"syscall"
	"time"
)

// stopPoll is how often a stopping holder's lock is looked at while it is
// given its time to go.
const stopPoll = 50 * time.Millisecond

// StopHolder hangs up the holder a record names: SIGHUP now, which a holder
// takes as Stop and passes on to its CLI. kill is the rest of it, for the
// caller to run — and wait on, or not: it waits up to after for the
// session's lock to be let go of, and sends SIGKILL if it is still held
// then. Each signal goes only while SameProcess says the pid is still that
// holder; when it is not the first time, the answer is ErrNotSameProcess
// and nothing was sent.
//
// after is longer than the time a holder has to stop its own CLI, so a
// holder that is doing that records how its CLI ended before anything kills
// it.
func StopHolder(r *Record, after time.Duration) (kill func(), err error) {
	m := r.Meta
	if m == nil {
		return nil, ErrNotSameProcess
	}
	if err := Signal(m.HolderPID, m.HolderStart, m.BootID, syscall.SIGHUP); err != nil {
		return nil, err
	}
	lock := LockPath(r.Dir)
	return func() {
		for deadline := time.Now().Add(after); time.Now().Before(deadline); time.Sleep(stopPoll) {
			if !Locked(lock) {
				return
			}
		}
		if Locked(lock) {
			_ = Signal(m.HolderPID, m.HolderStart, m.BootID, syscall.SIGKILL)
		}
	}, nil
}

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
