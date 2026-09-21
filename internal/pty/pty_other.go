//go:build !linux && !darwin

package pty

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
)

// Supported says this platform has no pseudo-terminal, so a caller can
// refuse a configuration at start-up rather than at the first terminal
// somebody asks for.
const Supported = false

// Start refuses, naming the platform: "not supported" on its own leaves
// whoever reads it wondering whether they mistyped something.
func Start(*exec.Cmd, uint16, uint16) (*os.File, error) { return nil, unsupported() }

// Resize refuses for the same reason, and is here so that a caller compiles
// everywhere and fails in one place.
func Resize(*os.File, uint16, uint16) error { return unsupported() }

func unsupported() error {
	return fmt.Errorf("%w: %s has none; the terminal runs on linux and macOS", ErrUnsupported, runtime.GOOS)
}
