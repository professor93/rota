package holdwire

import "errors"

// Stopping a holder from outside its link: what a daemon does to a holder
// it cannot speak to, and what stopping every session does with no daemon
// running. Said once, here, so every caller signals a recorded pid by the
// same rule. The signalling itself is stop_unix.go's, beside the signals it
// names; where held sessions do not run, stop_other.go refuses.

// ErrNotSameProcess is a record whose pid is no longer the process it
// names — the holder has gone and the kernel has handed its pid to some
// other program, or the record never named one. Nothing was signalled.
var ErrNotSameProcess = errors.New("holdwire: that pid is no longer the process its record names; nothing was signalled")
