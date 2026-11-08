//go:build !unix

package store

// journalOpenFlags add nothing where there are no such flags: the journal is
// still looked at before it is opened, and written only as a regular file.
const journalOpenFlags = 0
