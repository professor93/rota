//go:build linux || darwin

package main

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Reading a password without printing it is one ioctl on the terminal, twice:
// take the current settings, put them back without ECHO, read the line, and
// put the original settings back whatever happened.
//
// This is here rather than golang.org/x/term because rota depends on the
// standard library alone. The two constants differ by kernel — linux spells
// them TCGETS/TCSETS, the BSDs TIOCGETA/TIOCSETA — which is the whole reason
// this file is built per OS and its sibling says it cannot do it at all.

// termios reads the terminal's settings, and reports whether fd is a
// terminal at all: the ioctl failing with "not a tty" is the answer to both
// questions at once, so nothing else has to ask.
func termios(fd uintptr) (syscall.Termios, bool) {
	var t syscall.Termios
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, getTermios,
		uintptr(unsafe.Pointer(&t))); err != 0 {
		return t, false
	}
	return t, true
}

func setTermios(fd uintptr, t syscall.Termios) error {
	if _, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, setTermiosNow,
		uintptr(unsafe.Pointer(&t))); err != 0 {
		return err
	}
	return nil
}

// isTerminal reports whether standard input is a terminal, which is what
// decides between asking twice with no echo and reading one line.
func isTerminal(f *os.File) bool {
	_, ok := termios(f.Fd())
	return ok
}

// rawMode puts a terminal into the mode a full-screen program needs and
// hands back the one way out of it, which the caller must take on every path
// it can leave by.
//
// Raw is the terminal doing nothing on its own: no echo, no line editing, no
// turning ^C into a signal, no translating a newline on the way out. All of
// that still happens — it happens on the pseudo-terminal the CLI is on, one
// layer further in — and doing it twice is what makes a shared terminal
// print every keystroke back at you and swallow every interrupt.
//
// It is here beside noEcho because they are the same two ioctls with the
// same two names per kernel, and a second copy of those constants is a
// second place to get them wrong.
func rawMode(fd uintptr) (restore func(), err error) {
	before, ok := termios(fd)
	if !ok {
		return nil, errNotATerminal
	}
	raw := before
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	// One byte is enough to return from a read, and nothing is waited for:
	// a keystroke has to reach the CLI as it is typed, not when a timer
	// somewhere decides a line has been sitting long enough.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := setTermios(fd, raw); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = setTermios(fd, before) }) }, nil
}

// errNotATerminal is what rawMode answers for a descriptor that is not one,
// so a caller can say so in its own words rather than passing on an errno.
var errNotATerminal = errors.New("not a terminal")

// noEcho runs read with the terminal's echo off, and restores it after —
// including when read fails, and including when it panics, because a shell
// left with no echo is a shell somebody has to blindly type `stty sane` into.
func noEcho(f *os.File, read func() (string, error)) (string, error) {
	before, ok := termios(f.Fd())
	if !ok {
		return read()
	}
	quiet := before
	quiet.Lflag &^= syscall.ECHO
	if err := setTermios(f.Fd(), quiet); err != nil {
		return read()
	}
	defer setTermios(f.Fd(), before)
	return read()
}
