//go:build linux || darwin

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// Supported says this platform has a pseudo-terminal, so a caller can refuse
// a configuration at start-up rather than at the first terminal somebody
// asks for.
const Supported = true

// Start opens a pseudo-terminal of the given size, makes its slave the
// child's standard input, output and error and its controlling terminal,
// starts the process and returns the master.
//
// The slave is closed here on purpose. Two things hold it open — this
// process and the child — and only a slave nobody else holds turns the
// child's exit into an end-of-file on the master. Keeping it would leave the
// reader waiting for a process that has been gone for minutes.
//
// Setsid and Setctty go together: a process only takes a controlling
// terminal if it leads a session of its own, and without one the child gets
// no job control, so ^C in the browser would reach nothing.
func Start(cmd *exec.Cmd, cols, rows uint16) (*os.File, error) {
	master, name, err := open()
	if err != nil {
		return nil, err
	}
	slave, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, fmt.Errorf("opening the terminal %s: %w", name, err)
	}
	defer slave.Close()
	if err := Resize(master, cols, rows); err != nil {
		master.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	// Ctty is numbered in the child, where the slave is standard input
	// because it was given as Stdin above.
	cmd.SysProcAttr.Ctty = 0
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

// winsize is the kernel's struct winsize, the same four shorts in the same
// order on both platforms. It is declared here because package syscall
// exports the ioctl number and not the argument it takes.
type winsize struct {
	rows, cols     uint16
	xpixel, ypixel uint16
}

// Resize tells the terminal how big it is now. The child is sent SIGWINCH by
// the kernel, which is how a full-screen program learns to redraw.
func Resize(master *os.File, cols, rows uint16) error {
	ws := winsize{rows: rows, cols: cols}
	if err := control(master, syscall.TIOCSWINSZ, unsafe.Pointer(&ws)); err != nil {
		return fmt.Errorf("setting the terminal size: %w", err)
	}
	return nil
}

// control runs one ioctl on a file without taking its descriptor out of the
// runtime's poller. os.File.Fd would put the master into blocking mode, and
// a blocking master is one whose reader cannot be woken by closing it — the
// pump would sit in a read for a terminal that has already ended.
func control(f *os.File, req uintptr, arg unsafe.Pointer) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ioerr error
	if err := conn.Control(func(fd uintptr) { ioerr = ioctl(fd, req, uintptr(arg)) }); err != nil {
		return err
	}
	return ioerr
}

func ioctl(fd, req, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg); errno != 0 {
		return errno
	}
	return nil
}
