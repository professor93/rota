//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import "syscall"

// sessionOf is the session a process leads or belongs to; 0 is this one.
func sessionOf(pid int) (int, error) { return syscall.Getsid(pid) }
