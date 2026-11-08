package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// exitWatchable: Linux says when a process exits through a pidfd, or failing
// that through /proc.
const exitWatchable = true

// pidfdOpen is pidfd_open's number, the same on every architecture Linux has
// numbered alike since 5.3: all of Go's but the MIPS ones, which number from
// another base and are left to /proc.
const pidfdOpen = 434

// lookOnly makes exitOf look at /proc even where a pidfd is to be had, so a
// test can see that way answer too.
var lookOnly = false

// lookEvery is how often exitOf looks at /proc, where it has to.
var lookEvery = 100 * time.Millisecond

// zombieFor is how long a process must go on looking a zombie before /proc
// is believed, looked at again every zombieEvery. A thread other than the
// leader that execs — Go's syscall.Exec, from whatever thread it runs on —
// makes the leader a zombie for a moment while it takes over the pid, a few
// milliseconds when nothing is loaded: the process rota becomes the CLI
// through would read as exited. One that has exited stays a zombie until it
// is waited for, and is gone after.
var (
	zombieFor   = 50 * time.Millisecond
	zombieEvery = 10 * time.Millisecond
)

// readStat is /proc/<pid>/stat, a variable so a test can play what the
// kernel shows.
var readStat = func(pid int) ([]byte, error) {
	return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
}

// exitOf returns a wait that comes back once process pid has exited, a zombie
// included, and at once for one that has already gone.
//
// It is called while pid is still the process meant, so what it holds on to
// is that process and not its number: a pidfd names the process itself, and
// /proc is asked about the process with that pid that started when this one
// did. A pid used again by somebody else after an exit is never mistaken for
// the process that exited, and nor, through /proc, is an exec (see
// zombieFor).
func exitOf(pid int) (wait func(), err error) {
	if !lookOnly && !strings.HasPrefix(runtime.GOARCH, "mips") {
		fd, _, errno := syscall.Syscall(pidfdOpen, uintptr(pid), 0, 0)
		switch {
		case errno == syscall.ESRCH:
			return func() {}, nil
		case errno == 0:
			if wait, err := readable(int(fd)); err == nil {
				return wait, nil
			}
			_ = syscall.Close(int(fd))
		}
		// No pidfd here — a kernel before 5.3, a seccomp filter refusing
		// it — or no epoll to wait on it with: /proc answers instead.
	}
	raw, err := readStat(pid)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat("/proc/self/stat"); serr != nil {
			return nil, errNoHolder // no /proc either: nothing can be asked
		}
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	_, start, ok := parseStat(raw)
	if !ok {
		return nil, errors.New("unreadable /proc stat")
	}
	return func() {
		for {
			time.Sleep(lookEvery)
			switch lookAt(pid, start) {
			case exited:
				return
			case zombie:
				if stayedZombie(pid, start) {
					return
				}
			}
		}
	}, nil
}

// What one look at /proc found.
type seen int

const (
	running seen = iota
	exited       // gone, or another process has the pid
	zombie       // exited and not waited for yet, or in the middle of an exec
	unclear      // nothing to go on, a read that failed among them: look again
)

func lookAt(pid int, start string) seen {
	raw, err := readStat(pid)
	if errors.Is(err, fs.ErrNotExist) {
		return exited
	}
	if err != nil {
		return unclear
	}
	state, now, ok := parseStat(raw)
	switch {
	case !ok:
		return unclear
	case now != start:
		return exited
	case state == "Z" || state == "X" || state == "x":
		return zombie
	}
	return running
}

// stayedZombie looks again at a process that looked a zombie, and says
// whether it has exited: gone, or still a zombie zombieFor later. One that
// is running again was in the middle of an exec. Unclear looks wait.
func stayedZombie(pid int, start string) bool {
	for waited := time.Duration(0); ; waited += zombieEvery {
		time.Sleep(zombieEvery)
		s := lookAt(pid, start)
		switch {
		case s == exited:
			return true
		case s == running:
			return false
		case waited+zombieEvery >= zombieFor:
			return s == zombie
		}
	}
}

// readable waits on a pidfd, which is readable once its process has exited.
// The pidfd is close-on-exec, as pidfd_open always makes one.
func readable(pidfd int) (func(), error) {
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return nil, err
	}
	ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(pidfd)}
	if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, pidfd, &ev); err != nil {
		_ = syscall.Close(ep)
		return nil, err
	}
	return func() {
		defer syscall.Close(ep)
		defer syscall.Close(pidfd)
		events := make([]syscall.EpollEvent, 1)
		for {
			n, err := syscall.EpollWait(ep, events, -1)
			if err == syscall.EINTR {
				continue
			}
			if err != nil || n > 0 {
				return
			}
		}
	}, nil
}

// parseStat reads the state and the start time, in clock ticks since boot,
// out of /proc/<pid>/stat. The name in parentheses may hold anything, spaces
// and parentheses among them, so the fields are counted from the last ')'.
func parseStat(raw []byte) (state, start string, ok bool) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return "", "", false
	}
	// After the name: state, ppid, ... with starttime the 20th.
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 20 {
		return "", "", false
	}
	return f[0], f[19], true
}
