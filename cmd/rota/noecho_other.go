//go:build !linux && !darwin

package main

import "os"

// Where rota cannot turn a terminal's echo off, it does not pretend to.
// `rota serve passwd` then reads one line and says, before asking, that what
// is typed will be visible — which is a worse experience and an honest one.
// Windows can do this, through SetConsoleMode; it is left undone because
// nobody has a Windows terminal to try it in, and a password prompt that
// silently fails to hide anything is the one thing worse than this.

func isTerminal(*os.File) bool { return false }

// canHideInput says this platform cannot turn a terminal's echo off, so a
// pasted credential is not read from one here.
const canHideInput = false

func noEcho(_ *os.File, read func() (string, error)) (string, error) { return read() }
