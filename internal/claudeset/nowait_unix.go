//go:build unix

package claudeset

import "syscall"

// noWait is O_NONBLOCK, for opening what should be a regular file: one that
// turns out to be a FIFO opens at once instead of waiting for a writer, and is
// then refused by the check of what was opened.
const noWait = syscall.O_NONBLOCK
