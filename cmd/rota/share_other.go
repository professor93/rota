//go:build !linux && !darwin

package main

import (
	"fmt"
	"runtime"
)

// Where rota has no pseudo-terminal it cannot share a terminal, and it says
// so by name rather than by doing something approximate.
//
// Sharing is the local half of rota's terminal, and the terminal is a Unix
// feature: it needs a pseudo-terminal for the CLI, a raw mode for the window
// rota is sitting at, and a signal when that window is dragged. A Windows
// console has all three under a different API, and none of them is reached
// the way the two files beside this one reach them. This is rota's platform
// order rather than an oversight, and the message says which platforms do
// have it so that nobody goes looking for a flag they typed correctly.
func (c *cli) startShare(sharePlan) (int, error) {
	return 0, fmt.Errorf("--share needs a pseudo-terminal, and %s has none; rota shares a terminal on linux and macOS", runtime.GOOS)
}
