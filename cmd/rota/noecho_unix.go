//go:build linux || darwin

package main

import (
	"os"
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
