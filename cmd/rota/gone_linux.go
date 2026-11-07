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

// exitOf returns a wait that comes back once process pid has exited, a zombie
// included, and at once for one that has already gone.
//
// It is called while pid is still the process meant, so what it holds on to
// is that process and not its number: a pidfd names the process itself, and
// /proc is asked about the process with that pid that started when this one
// did. A pid used again by somebody else after an exit is never mistaken for
// the process that exited.
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
	start, err := startOf(pid)
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Stat("/proc/self/stat"); serr != nil {
			return nil, errNoHolder // no /proc either: nothing can be asked
		}
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	return func() {
		for {
			time.Sleep(lookEvery)
			if now, err := startOf(pid); err != nil || now != start {
				return
			}
		}
	}, nil
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

// startOf is when process pid started, in clock ticks since boot, as
// /proc/<pid>/stat says; an error for one that has exited, a zombie included.
// The name in parentheses may hold anything, spaces and parentheses among
// them, so the fields are counted from the last ')'.
func startOf(pid int) (string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return "", errors.New("unreadable /proc stat")
	}
	// After the name: state, ppid, ... with starttime the 20th.
	f := strings.Fields(string(raw[i+1:]))
	if len(f) < 20 {
		return "", errors.New("unreadable /proc stat")
	}
	if f[0] == "Z" || f[0] == "X" || f[0] == "x" {
		return "", fs.ErrNotExist
	}
	return f[19], nil
}
