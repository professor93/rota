//go:build !unix && !windows

package store

import "os"

// lockFile is a no-op outside Unix: concurrent rota processes are not
// serialized there, so run one at a time.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}

// keepAcrossExec does nothing outside Unix, where there is no handover to
// survive: the CLI runs as a child and this process stays to hold the lock.
func keepAcrossExec(*os.File) error { return nil }

// tryLockFile always succeeds outside Unix, for the same reason lockFile does
// nothing there: run one at a time.
func tryLockFile(path string) (*os.File, bool, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	return f, err == nil, err
}
