//go:build linux

package proc

import (
	"os"
	"time"
)

// kernel is this machine's /proc.
var kernel = Procfs{Read: os.ReadFile}

// StartTime is when a process started, which tells a pid from the same
// number given to a later process. On Linux it is Procfs.StartTime, which
// moves when the clock is set; StartTicks is what does not.
func StartTime(pid int) (time.Time, error) { return kernel.StartTime(pid) }

// StartTicks is when a process started in clock ticks since boot, which
// no setting of the clock moves: field 22 of /proc/<pid>/stat.
func StartTicks(pid int) (int64, error) { return kernel.StartTicks(pid) }

// Parent is a process's parent: field 4 of /proc/<pid>/stat.
func Parent(pid int) (int, error) { return kernel.Parent(pid) }

// BootID names this boot of the machine: a pid and a start time recorded
// before a reboot say nothing about the processes after it.
func BootID() (string, error) { return kernel.BootID() }
