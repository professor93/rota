// Package pty opens a pseudo-terminal and starts a process on it, using the
// standard library alone.
//
// A program that only reads a pipe is not at a terminal, and it knows: it
// turns off colour, stops drawing a prompt, and answers as if it were being
// scripted. Something meant to be typed at has to be given a terminal, and a
// pseudo-terminal is the pair of devices that makes one — a master this
// process reads and writes, and a slave the child believes is its console.
//
// There is no portable way to ask for one, so this is per-platform and
// build-tagged: linux and darwin have an implementation, and everywhere else
// both calls refuse by name. That is rota's platform order rather than an
// oversight — the terminal is a Unix feature, and a Windows console is a
// different thing with a different API (ConPTY) that nothing here needs yet.
package pty

import "errors"

// ErrUnsupported is what Start and Resize wrap where rota has no
// pseudo-terminal. It is a sentinel so a caller can tell "this machine
// cannot" from "this terminal would not start", which are different answers
// to whoever asked.
var ErrUnsupported = errors.New("rota has no pseudo-terminal on this platform")
