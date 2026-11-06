//go:build linux

package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's start time. The
// kernel fixes it at 100 for every architecture's user ABI, whatever HZ it
// was built with, so it is a constant rather than a sysconf the standard
// library cannot make.
const clockTicks = 100

// StartTime is when a process started, which tells a pid from the same
// number given to a later process. On Linux it is field 22 of
// /proc/<pid>/stat, ticks since boot, added to the boot time /proc/stat
// gives; both are coarse, so two readings may differ by a second.
func StartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("proc: no process %d", pid)
	}
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, err
	}
	ticks, err := startTicks(raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("proc: /proc/%d/stat: %w", pid, err)
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	return boot.Add(time.Duration(ticks) * (time.Second / clockTicks)), nil
}

// startTicks reads field 22. The second field is the command's name in
// parentheses, which may itself hold spaces and parentheses, so the count
// starts after the last closing one: what follows it is field 3.
func startTicks(stat []byte) (int64, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, errors.New("no command name")
	}
	fields := strings.Fields(string(stat[i+1:]))
	const at = 22 - 3
	if len(fields) <= at {
		return 0, errors.New("too few fields")
	}
	return strconv.ParseInt(fields[at], 10, 64)
}

// bootTime is the btime line of /proc/stat: whole seconds since the epoch.
func bootTime() (time.Time, error) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.Lines(string(raw)) {
		if rest, ok := strings.CutPrefix(line, "btime "); ok {
			sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
			if err != nil {
				return time.Time{}, fmt.Errorf("proc: /proc/stat btime: %w", err)
			}
			return time.Unix(sec, 0), nil
		}
	}
	return time.Time{}, errors.New("proc: /proc/stat has no btime")
}

// BootID names this boot of the machine: a pid and a start time recorded
// before a reboot say nothing about the processes after it.
func BootID() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", errors.New("proc: the boot id is empty")
	}
	return id, nil
}
