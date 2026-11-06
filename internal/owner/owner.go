// Package owner is the one rule rota has for a file nobody but its owner may
// use — server.toml, a token file, a server's log — and the one sentence it
// refuses such a file in.
package owner

import (
	"fmt"
	"io/fs"
	"runtime"
)

// Only refuses a file that group or others can read or write, and says the
// one command that fixes it. Windows has neither those bits nor that
// convention, so the rule is unix's.
func Only(path string, mode fs.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if perm := mode.Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s is readable or writable by other users (mode %04o); "+
			"it is its owner's alone, so fix it with: chmod 600 %s", path, perm, path)
	}
	return nil
}
