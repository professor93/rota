//go:build !unix && !windows

package sessions

import "os"

// lockFile is a no-op outside Unix, matching the account store: concurrent
// rota processes are not serialized there. Runs inside one process still are,
// by the mutex the caller holds.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
