//go:build linux

package main

import "syscall"

// Linux calls the two ioctls TCGETS and TCSETS.
const (
	getTermios    = syscall.TCGETS
	setTermiosNow = syscall.TCSETS
)
