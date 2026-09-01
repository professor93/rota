//go:build !unix

package sessions

import (
	"errors"
	"os"
	"syscall"
)

// alive reports whether a process id still names a running process.
//
// Signal 0 — the unix probe — is not a question Windows answers: sending it
// through os.Process.Signal returns "not supported", which would read every
// live run as dead and prune the whole registry. os.FindProcess is the probe
// here instead: on Windows it opens the process, so an id that no longer
// names one is an error rather than a handle.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	if err == nil {
		return true
	}
	// Access denied is proof of life — a process this user may not open is
	// still a process — mirroring the unix probe's permission rule.
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 5 /* ERROR_ACCESS_DENIED */
}
