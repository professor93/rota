//go:build !linux && !darwin

package proc

import "time"

// StartTime is not known here.
func StartTime(pid int) (time.Time, error) { return time.Time{}, ErrPlatform }

// BootID is not known here.
func BootID() (string, error) { return "", ErrPlatform }
