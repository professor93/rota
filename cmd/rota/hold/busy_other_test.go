//go:build !linux && !darwin

package hold

import (
	"testing"
	"time"
)

var began = time.Now()

// busy is the time on the clock since the test binary began, where the
// process's CPU time is not read: there a loaded machine still counts
// against a test that bounds how long its code takes.
func busy(t *testing.T) time.Duration {
	t.Helper()
	return time.Since(began)
}
