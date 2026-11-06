//go:build linux || darwin

package hold

import (
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"github.com/professor93/rota/internal/pty"
)

// killSignal ends a CLI that must not outlive a holder that failed to start.
const killSignal = syscall.SIGKILL

// ptyChild is the CLI on its pseudo-terminal: the master to read and write,
// and the process to signal and wait for.
type ptyChild struct {
	master *os.File
	cmd    *exec.Cmd
	once   sync.Once
	code   int
}

// startChild starts path on a pseudo-terminal of its own, in a session of
// its own, with exactly env, in dir.
func startChild(path string, args, env []string, dir string, cols, rows uint16) (*ptyChild, error) {
	cmd := exec.Command(path, args...)
	cmd.Env, cmd.Dir = env, dir
	master, err := pty.Start(cmd, cols, rows)
	if err != nil {
		return nil, err
	}
	return &ptyChild{master: master, cmd: cmd, code: -1}, nil
}

func (c *ptyChild) Read(p []byte) (int, error)     { return c.master.Read(p) }
func (c *ptyChild) Write(p []byte) (int, error)    { return c.master.Write(p) }
func (c *ptyChild) Resize(cols, rows uint16) error { return pty.Resize(c.master, cols, rows) }
func (c *ptyChild) Signal(sig os.Signal) error     { return c.cmd.Process.Signal(sig) }
func (c *ptyChild) Pid() int                       { return c.cmd.Process.Pid }
func (c *ptyChild) Close() error                   { return c.master.Close() }

// Wait is how the CLI ended: its exit status, or -1 when a signal ended it,
// as a terminal the server runs itself reports it.
func (c *ptyChild) Wait() int {
	c.once.Do(func() {
		_ = c.cmd.Wait()
		if c.cmd.ProcessState != nil {
			c.code = c.cmd.ProcessState.ExitCode()
		}
	})
	return c.code
}

// atTerminal says whether in is a terminal: somebody typing, not a daemon.
func atTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	conn, err := f.SyscallConn()
	if err != nil {
		return false
	}
	tty := false
	_ = conn.Control(func(fd uintptr) {
		var t syscall.Termios
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, readTermios, uintptr(unsafe.Pointer(&t)))
		tty = errno == 0
	})
	return tty
}

// leadsSession says whether this process leads a session of its own, as a
// holder the daemon started does: one that does not would be stopped with
// whatever terminal or service it was started from.
func leadsSession() bool {
	sid, err := sessionOf()
	return err == nil && sid == os.Getpid()
}

// quiet points standard output and error at /dev/null.
func quiet() error {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	for _, fd := range []int{1, 2} {
		if err := dupOnto(int(null.Fd()), fd); err != nil {
			return err
		}
	}
	return nil
}
