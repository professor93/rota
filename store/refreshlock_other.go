//go:build !linux && !darwin

package store

import (
	"io/fs"
	"os"
	"time"
)

// lockDir is the refresh lock rota made, reached here by its name at every
// beat: a lock somebody makes at that name between a beat's look and its
// touch is touched, once. It is still never taken for rota's, as the beat
// keeps what it reads back only while that is the directory rota made
// (os.SameFile), so it is not removed by the release either.
type lockDir struct{ path string }

func openLockDir(path string, _ fs.FileInfo) (lockDir, error) { return lockDir{path}, nil }

func (d lockDir) stat() (fs.FileInfo, error) { return os.Lstat(d.path) }

func (d lockDir) close() {}

func (d lockDir) touch(t time.Time) error { return os.Chtimes(d.path, t, t) }
