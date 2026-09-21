//go:build darwin

package pty

import (
	"bytes"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// open asks the kernel for a fresh pair and says where the slave is.
//
// macOS allocates through the same /dev/ptmx, but the three steps after it
// are its own: grant hands the slave to this user, unlock clears the guard
// that keeps a half-prepared terminal from being opened, and gname asks for
// the path rather than a number — the slave is /dev/ttysNNN here, not
// /dev/pts/N, and only the kernel knows which.
func open() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("opening /dev/ptmx: %w", err)
	}
	for _, step := range []struct {
		what string
		req  uintptr
	}{
		{"granting", syscall.TIOCPTYGRANT},
		{"unlocking", syscall.TIOCPTYUNLK},
	} {
		if err := control(master, step.req, nil); err != nil {
			master.Close()
			return nil, "", fmt.Errorf("%s the terminal: %w", step.what, err)
		}
	}
	// The kernel writes a NUL-terminated path into a buffer of this fixed
	// size; anything shorter is what the ioctl is documented to refuse.
	var name [128]byte
	if err := control(master, syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0])); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("naming the terminal: %w", err)
	}
	path := string(name[:])
	if end := bytes.IndexByte(name[:], 0); end >= 0 {
		path = string(name[:end])
	}
	return master, path, nil
}
