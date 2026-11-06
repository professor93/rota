//go:build !linux && !darwin

package api

import (
	"os/exec"
	"syscall"
)

// Held terminals run on linux and macOS only, and New refuses HoldCommand
// anywhere else; these are never reached.

func detachHolder(*exec.Cmd) {}

func (h *heldTerm) signal(syscall.Signal) {}
