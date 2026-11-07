//go:build !unix

package store

// noWait adds nothing where there is no FIFO to wait on: a path is opened as
// it always was, and what was opened is checked all the same.
const noWait = 0
