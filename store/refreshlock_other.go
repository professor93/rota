//go:build !linux && !darwin

package store

import (
	"io/fs"
	"os"
	"time"
)

// lockDir is Claude Code's refresh lock as rota found it at a beat. Here it
// is reached by its name each time, as before: a lock somebody makes at that
// name between rota's look and its touch can still be touched, and on
// Windows, where os.SameFile reads a directory's identity by its name when
// first asked, taken for rota's after.
type lockDir struct{ path string }

func openLockDir(path string) (lockDir, error) {
	_, err := os.Lstat(path)
	return lockDir{path}, err
}

func (d lockDir) stat() (fs.FileInfo, error) { return os.Lstat(d.path) }

func (d lockDir) close() {}

func (d lockDir) touch(t time.Time) error { return os.Chtimes(d.path, t, t) }
