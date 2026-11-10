//go:build !linux

package proc

// Cmdline is read only on Linux, the one system whose start times move with
// the clock and so may need a second look at what a process is.
func Cmdline(pid int) ([]string, error) { return nil, ErrPlatform }
