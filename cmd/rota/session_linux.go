package main

import "syscall"

// sessionOf is the session a process leads or belongs to; 0 is this one.
// Linux's syscall package has no Getsid of its own.
func sessionOf(pid int) (int, error) {
	sid, _, errno := syscall.RawSyscall(syscall.SYS_GETSID, uintptr(pid), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(sid), nil
}
