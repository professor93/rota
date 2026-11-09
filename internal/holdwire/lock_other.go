//go:build !linux && !darwin

package holdwire

import "os"

// Held sessions and the daemon run on linux and macOS; elsewhere no lock is
// taken and none is ever held.

func tryLock(path string) (*os.File, error) { return nil, ErrPlatform }

func unlock(f *os.File) {}

// Locked is never true here.
func Locked(path string) bool { return false }
