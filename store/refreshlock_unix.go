//go:build linux || darwin

package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// lockDir is the refresh lock rota made, open from the take until the
// release, so that every beat's touch and reading back are of that one
// directory, whatever is made at its name meanwhile, and need no new
// descriptor: a beat that could not open one would let the lock go stale.
type lockDir struct{ f *os.File }

// openLockDir opens the directory just made at path, as made. Only a
// directory, and not through a link: a FIFO or a link at the name fails here
// at once rather than block, and is no lock of rota's.
func openLockDir(path string, made fs.FileInfo) (lockDir, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return lockDir{}, err
	}
	fi, err := f.Stat()
	switch {
	case err != nil:
		_ = f.Close()
		return lockDir{}, fmt.Errorf("could not be read back: %w", err)
	case !os.SameFile(fi, made):
		_ = f.Close()
		return lockDir{}, errors.New("another directory took its place")
	}
	return lockDir{f}, nil
}

func (d lockDir) stat() (fs.FileInfo, error) { return d.f.Stat() }

func (d lockDir) close() { _ = d.f.Close() }

// touch gives the directory t for its mtime: a lock somebody made at its
// name meanwhile is not touched.
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
