//go:build !linux && !darwin

package proc

import "time"

// StartTime is not known here.
func StartTime(pid int) (time.Time, error) { return time.Time{}, ErrPlatform }

// StartTicks is not known here either.
func StartTicks(pid int) (int64, error) { return 0, ErrPlatform }

// Parent is not known here.
func Parent(pid int) (int, error) { return 0, ErrPlatform }

// BootID is not known here.
func BootID() (string, error) { return "", ErrPlatform }
