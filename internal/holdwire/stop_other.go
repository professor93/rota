//go:build !linux && !darwin

package holdwire

import (
	"syscall"
	"time"
)

// StopHolder refuses: there are no holders here to stop.
func StopHolder(*Record, time.Duration) (func(), error) { return nil, ErrPlatform }

// Signal refuses: there are no holders here to signal.
func Signal(int, time.Time, string, syscall.Signal) error { return ErrPlatform }
