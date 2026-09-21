//go:build darwin

package main

import "syscall"

// The BSDs, macOS among them, call the same two TIOCGETA and TIOCSETA.
const (
	getTermios    = syscall.TIOCGETA
	setTermiosNow = syscall.TIOCSETA
)
