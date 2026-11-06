//go:build !linux && !darwin

package holdwire

import (
	"syscall"
	"time"
)

// Signal refuses: there are no holders here to signal.
func Signal(int, time.Time, string, syscall.Signal) error { return ErrPlatform }
