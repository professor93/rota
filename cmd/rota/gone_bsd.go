//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"syscall"
	"time"

	"github.com/professor93/rota/internal/proc"
)

// exitWatchable: kqueue says when a process exits.
const exitWatchable = true

// exitOf returns a wait that comes back once process pid has exited, a zombie
// included, and at once for one that has already gone.
//
// It is called while pid is still the process meant, and kqueue holds on to
// that process rather than its number, so a pid used again by somebody else
// after an exit is never mistaken for the process that exited.
func exitOf(pid int) (wait func(), err error) {
	kq, err := syscall.Kqueue()
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(kq)
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	ev.Fflags = syscall.NOTE_EXIT
	if _, err := syscall.Kevent(kq, []syscall.Kevent_t{ev}, nil, nil); err != nil {
		_ = syscall.Close(kq)
		if err == syscall.ESRCH {
			return func() {}, nil
		}
		return nil, err
	}
	return func() {
		defer syscall.Close(kq)
		events := make([]syscall.Kevent_t, 1)
		for {
			n, err := syscall.Kevent(kq, nil, events, nil)
			if err == syscall.EINTR {
				continue
			}
			if n > 0 {
				return
			}
			if err != nil {
				// kqueue gave up on a process it had taken: ask by its pid
				// from here on. That can follow the pid to a process given
				// it after the exit, so the claim may be held too long, never
				// let go too soon.
				for proc.Alive(pid) {
					time.Sleep(time.Second)
				}
				return
			}
		}
	}, nil
}
