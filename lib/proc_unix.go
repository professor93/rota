//go:build unix

package rota

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// killGrace is how long a CLI that was asked to stop gets before it is
// killed. Claude Code rotates its refresh token, and a process killed between
// the provider accepting a refresh and the store saving it destroys the login
// for everything in its home; it finishes that and exits when asked. Shorter
// than waitDelay, so the group is gone before exec's own last resort reaches
// for the CLI alone. A variable so a test need not wait.
var killGrace = 3 * time.Second

// settlePoll is how often a stop that is waiting for a group looks at it.
const settlePoll = 50 * time.Millisecond

// setPgid puts the CLI in its own process group, so killing it takes the
// helpers it spawned with it.
func setPgid(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopper ends one CLI and the helpers in its process group: SIGTERM at once
// and SIGKILL when the grace is over, once however often it is asked for. The
// kill is on a timer rather than a wait, so whoever asks goes straight on to
// what stopping needs, reading what the CLI still prints above all: a kill that
// waited first could not be relied on to unblock Wait.
type stopper struct {
	c  *exec.Cmd
	mu sync.Mutex
	// over is set once the CLI has been waited for: its pid is not this
	// process's to signal any more.
	over   bool
	kill   *time.Timer
	killed chan struct{}
}

// newStopper makes the stopper for a command not yet started, and puts it in
// place of exec's own answer to the context, which is SIGKILL to the CLI alone.
func newStopper(c *exec.Cmd) *stopper {
	s := &stopper{c: c, killed: make(chan struct{})}
	c.Cancel = func() error {
		s.stop()
		return nil
	}
	return s
}

// stop asks the group to end, and arranges for it to be killed if it does not.
func (s *stopper) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kill != nil || s.over || s.c.Process == nil {
		return
	}
	pid := s.c.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		_ = s.c.Process.Signal(syscall.SIGTERM)
	}
	s.kill = time.AfterFunc(killGrace, func() {
		defer close(s.killed)
		if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			_ = s.c.Process.Kill()
		}
	})
}

// settle is what a stop owes once the CLI has been waited for. Nothing is left
// to do when the group is gone, and it is looked for again and again, because
// what the CLI started usually goes a moment after it does. A helper that
// stayed behind keeps the group, and has the rest of the grace, because it may
// be the one in the middle of a refresh; the kill is then waited for, a bounded
// wait, so a stopped run still leaves nothing running when it returns.
func (s *stopper) settle() {
	s.mu.Lock()
	s.over = true
	kill := s.kill
	s.mu.Unlock()
	if kill == nil {
		return
	}
	look := time.NewTicker(settlePoll)
	defer look.Stop()
	for {
		if groupGone(s.c.Process.Pid) && kill.Stop() {
			close(s.killed)
			return
		}
		select {
		case <-s.killed:
			return
		case <-look.C:
		}
	}
}

// groupGone reports whether nothing that can still act is left in a process
// group. A zombie stays in its group until it is reaped, and under a
// container's first process, which reaps nothing, that is for ever: a group of
// only zombies has gone too. Telling one takes /proc, so where there is none
// such a group is taken for alive and costs the rest of the grace; that is
// macOS and the BSDs, where an init always reaps.
func groupGone(group int) bool {
	if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
		return true
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	zombies := false
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // ended between the listing and now
		}
		state, g, ok := procStat(raw)
		switch {
		case !ok || g != group:
		case state != 'Z':
			return false
		default:
			zombies = true
		}
	}
	return zombies
}

// procStat reads a process's state and process group from its line in
// /proc/<pid>/stat. They are counted from the last parenthesis, because the
// name before it is the process's own and may hold anything.
func procStat(raw []byte) (state byte, group int, ok bool) {
	i := bytes.LastIndexByte(raw, ')')
	if i < 0 {
		return 0, 0, false
	}
	fields := bytes.Fields(raw[i+1:]) // state, parent, group, ...
	if len(fields) < 3 || len(fields[0]) != 1 {
		return 0, 0, false
	}
	group, err := strconv.Atoi(string(fields[2]))
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], group, true
}
