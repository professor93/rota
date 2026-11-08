//go:build unix

package store

import "syscall"

// journalOpenFlags keep a journal open from following a link put at its path
// or waiting on a pipe there: either fails, and the line is not written.
const journalOpenFlags = syscall.O_NOFOLLOW | syscall.O_NONBLOCK
