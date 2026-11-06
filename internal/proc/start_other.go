//go:build !linux && !darwin

package proc

import (
	"errors"
	"time"
)

// errNoHeld is the answer on a platform held sessions do not run on.
var errNoHeld = errors.New("held sessions and the rota daemon run on linux and macOS")

// StartTime is not known here.
func StartTime(pid int) (time.Time, error) { return time.Time{}, errNoHeld }

// BootID is not known here.
func BootID() (string, error) { return "", errNoHeld }
