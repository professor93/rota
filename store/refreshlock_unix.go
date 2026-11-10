//go:build linux || darwin

package store

import (
	"io/fs"
	"os"
	"time"
)

// lockDir is Claude Code's refresh lock as rota found it at a beat: the
// directory itself, open, so that what rota reads of it and the mtime it
// gives it are of one directory, whatever is made at its name in between.
type lockDir struct{ f *os.File }

func openLockDir(path string) (lockDir, error) {
	f, err := os.Open(path)
	return lockDir{f}, err
}

func (d lockDir) stat() (fs.FileInfo, error) { return d.f.Stat() }

func (d lockDir) close() { _ = d.f.Close() }

// touch gives the open directory t for its mtime: a lock somebody made at
// its name since it was opened is not touched.
func (d lockDir) touch(t time.Time) error {
	c, err := d.f.SyscallConn()
	if err != nil {
		return err
	}
	var touched error
	if err := c.Control(func(fd uintptr) { touched = futimes(int(fd), t) }); err != nil {
		return err
	}
	return touched
}
