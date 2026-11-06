//go:build darwin

package proc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"syscall"
	"time"
	"unsafe"
)

// StartTime is when a process started, which tells a pid from the same
// number given to a later process. On macOS it is the first timeval of
// kern.proc.pid.<pid> — kinfo_proc opens with extern_proc, whose first
// member is p_starttime — to the second.
//
// The standard library's Sysctl takes a name and cannot name a pid, so the
// MIB is passed as numbers: CTL_KERN, KERN_PROC, KERN_PROC_PID.
func StartTime(pid int) (time.Time, error) {
	if pid <= 0 || pid > math.MaxInt32 {
		return time.Time{}, fmt.Errorf("proc: no process %d", pid)
	}
	mib := [4]int32{1 /* CTL_KERN */, 14 /* KERN_PROC */, 1 /* KERN_PROC_PID */, int32(pid)}
	var buf [1024]byte // kinfo_proc is 648 bytes on both of macOS's architectures
	n := uintptr(len(buf))
	if _, _, errno := syscall.Syscall6(syscall.SYS___SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0, 0); errno != 0 {
		return time.Time{}, fmt.Errorf("proc: kern.proc.pid.%d: %w", pid, errno)
	}
	// A pid nothing has is not an error to the kernel: it answers with
	// nothing at all.
	if n < 8 {
		return time.Time{}, fmt.Errorf("proc: no process %d", pid)
	}
	sec := int64(binary.NativeEndian.Uint64(buf[:8]))
	if sec <= 0 {
		return time.Time{}, fmt.Errorf("proc: process %d has no start time", pid)
	}
	return time.Unix(sec, 0), nil
}

// BootID names this boot of the machine: a pid and a start time recorded
// before a reboot say nothing about the processes after it. On macOS it is
// kern.bootsessionuuid, a UUID the kernel makes once per boot. The boot time
// would not do: setting the clock moves kern.boottime, and a moved clock is
// not a reboot.
func BootID() (string, error) {
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", fmt.Errorf("proc: kern.bootsessionuuid: %w", err)
	}
	if id == "" {
		return "", errors.New("proc: kern.bootsessionuuid is empty")
	}
	return id, nil
}
