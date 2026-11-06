package holdwire

import (
	"errors"
	"syscall"
	"time"
)

// Stopping a holder from outside its link: what a daemon does to a holder
// it cannot speak to, and what stopping every session does with no daemon
// running. Said once, here, so every caller signals a recorded pid by the
// same rule.

// ErrNotSameProcess is a record whose pid is no longer the process it
// names — the holder has gone and the kernel has handed its pid to some
// other program, or the record never named one. Nothing was signalled.
var ErrNotSameProcess = errors.New("holdwire: that pid is no longer the process its record names; nothing was signalled")

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
