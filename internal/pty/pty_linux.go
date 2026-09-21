//go:build linux

package pty

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// open asks the kernel for a fresh pair and says where the slave is.
//
// Linux hands out the pair through one multiplexer, /dev/ptmx: opening it is
// the allocation, and the slave has to be asked for by number afterwards.
// The lock has to be cleared before the slave can be opened at all — it is
// there so that a program cannot be handed a terminal somebody else is still
// setting up.
func open() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", fmt.Errorf("opening /dev/ptmx: %w", err)
	}
	var unlock int32 // zero unlocks; anything else locks it again
	if err := control(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlocking the terminal: %w", err)
	}
	var n uint32
	if err := control(master, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("naming the terminal: %w", err)
	}
	return master, "/dev/pts/" + strconv.FormatUint(uint64(n), 10), nil
}
