package proc

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Procfs is a Linux /proc, read through Read, its links through Link. How
// each of its files is read is written once, here, and built everywhere, so
// a test can be the kernel on any system; on Linux the functions this
// package exports read the real one.
type Procfs struct {
	Read func(name string) ([]byte, error)
	Link func(name string) (string, error)
}

// clockTicks is USER_HZ, the unit of /proc/<pid>/stat's start time. The
// kernel fixes it at 100 for every architecture's user ABI, whatever HZ it
// was built with, so it is a constant rather than a sysconf the standard
// library cannot make.
const clockTicks = 100

// StartTime is when a process started by the clock: its start in ticks
// since boot, added to the boot time /proc/stat gives. Both are coarse, so
// two readings may differ by a second. It moves when the clock is set: the
// boot time is the clock less the time since boot, so a step of the clock
// steps it too. StartTicks does not move.
func (p Procfs) StartTime(pid int) (time.Time, error) {
	ticks, err := p.StartTicks(pid)
	if err != nil {
		return time.Time{}, err
	}
	boot, err := p.bootTime()
	if err != nil {
		return time.Time{}, err
	}
	return boot.Add(time.Duration(ticks) * (time.Second / clockTicks)), nil
}

// StartTicks is when a process started, in clock ticks since boot: field
// 22 of /proc/<pid>/stat. The kernel recorded it once, from a clock nobody
// sets, so it is the same number every time it is read, whatever the clock
// has done since.
func (p Procfs) StartTicks(pid int) (int64, error) {
	fields, err := p.stat(pid)
	if err != nil {
		return 0, err
	}
	const at = 22 - 3
	if len(fields) <= at {
		return 0, fmt.Errorf("proc: /proc/%d/stat: too few fields", pid)
	}
	ticks, err := strconv.ParseInt(fields[at], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("proc: /proc/%d/stat: %w", pid, err)
	}
	return ticks, nil
}

// Parent is the process a process's parent is now: field 4 of
// /proc/<pid>/stat. A process whose parent has gone has been handed to init,
// or to whichever of its ancestors reaps orphans, and names that one.
func (p Procfs) Parent(pid int) (int, error) {
	fields, err := p.stat(pid)
	if err != nil {
		return 0, err
	}
	const at = 4 - 3
	if len(fields) <= at {
		return 0, fmt.Errorf("proc: /proc/%d/stat: too few fields", pid)
	}
	ppid, err := strconv.Atoi(fields[at])
	if err != nil {
		return 0, fmt.Errorf("proc: /proc/%d/stat: %w", pid, err)
	}
	return ppid, nil
}

// Cmdline is the arguments a process was started with, or has since given
// itself: /proc/<pid>/cmdline, split at its NULs. A process with none, a
// kernel thread or a zombie, gives an empty list.
func (p Procfs) Cmdline(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("proc: no process %d", pid)
	}
	raw, err := p.Read("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimRight(raw, "\x00")
	if len(raw) == 0 {
		return nil, nil
	}
	return strings.Split(string(raw), "\x00"), nil
}

// Names is every name a process goes by: its arguments (Cmdline), the
// command name the kernel keeps for it (/proc/<pid>/comm, which a process
// may set as its title), and the program it runs (the target of
// /proc/<pid>/exe, which a renamed link or a wrapper's exec does not hide).
// Whatever of these cannot be read is left out, as another user's exe
// usually is; only when none can be read is it an error.
func (p Procfs) Names(pid int) ([]string, error) {
	args, err := p.Cmdline(pid)
	names := args
	if comm, cerr := p.Read("/proc/" + strconv.Itoa(pid) + "/comm"); cerr == nil {
		names = append(names, strings.TrimRight(string(comm), "\n"))
		err = nil
	}
	if p.Link != nil {
		if exe, lerr := p.Link("/proc/" + strconv.Itoa(pid) + "/exe"); lerr == nil {
			names = append(names, exe)
			err = nil
		}
	}
	if err != nil {
		return nil, err
	}
	return names, nil
}

// stat is /proc/<pid>/stat's fields from the third on. The second is the
// command's name in parentheses, which may itself hold spaces and
// parentheses, so the count starts after the last closing one: what follows
// it is field 3.
func (p Procfs) stat(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("proc: no process %d", pid)
	}
	raw, err := p.Read("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return nil, fmt.Errorf("proc: /proc/%d/stat: no command name", pid)
	}
	return strings.Fields(string(raw[i+1:])), nil
}

// bootTime is the btime line of /proc/stat: whole seconds since the epoch.
func (p Procfs) bootTime() (time.Time, error) {
	raw, err := p.Read("/proc/stat")
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
func (p Procfs) BootID() (string, error) {
	raw, err := p.Read("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", errors.New("proc: the boot id is empty")
	}
	return id, nil
}
