//go:build !linux && !darwin

package fakecli

// There is no pseudo-terminal to be at on these platforms, so the fake's
// interactive mode is never reached there. The two calls exist so that
// everything else in this package still compiles everywhere.

func termSize() (cols, rows uint16) { return 0, 0 }

func watchResize(func()) {}
