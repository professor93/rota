package store

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Backend is where a Store keeps its bytes. The rota library itself has no
// opinion about storage at all — this package is one implementation of it,
// and an application that keeps accounts in a database can skip the package
// entirely and call the rota verbs directly.
type Backend interface {
	// Load returns the stored blob. A missing store is (nil, nil), not an
	// error: that is a first run.
	Load() ([]byte, error)
	// Save replaces the blob. It must be atomic: the blob holds live
	// refresh tokens and a torn write loses accounts.
	Save(blob []byte) error
	// Lock takes exclusive access until the returned function is called.
	// Concurrent rota processes must not interleave their writes, or one
	// overwrites a refresh token the other just rotated.
	Lock() (unlock func(), err error)
	// HomeRoot is the directory under which per-account CLI homes are
	// staged. It must be on a real filesystem — the CLIs read files from
	// it — and readable only by this user.
	HomeRoot() string
}

// FileBackend keeps the store in <Dir>/accounts.json (0600, written
// atomically), locks <Dir>/.lock, and stages homes under <Dir>/homes.
type FileBackend struct{ Dir string }

// DefaultDir is $ROTA_HOME, or ~/.rota.
func DefaultDir() (string, error) {
	if d := os.Getenv("ROTA_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("no home directory; set ROTA_HOME")
	}
	return filepath.Join(home, ".rota"), nil
}

// NewFileBackend prepares dir ("" for DefaultDir) at 0700.
func NewFileBackend(dir string) (*FileBackend, error) {
	if dir == "" {
		var err error
		if dir, err = DefaultDir(); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileBackend{Dir: dir}, nil
}

func (f *FileBackend) path() string { return filepath.Join(f.Dir, "accounts.json") }

func (f *FileBackend) Load() ([]byte, error) {
	f.sweep()
	raw, err := os.ReadFile(f.path())
	if os.IsNotExist(err) {
		return nil, nil
	}
	return raw, err
}

// sweep removes temp files a crash left behind between write and rename.
// They are mode 0600 and hold live refresh tokens, so leaving them to
// accumulate is both untidy and a small hazard.
func (f *FileBackend) sweep() {
	leftovers, err := filepath.Glob(f.path() + "-*.tmp")
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, name := range leftovers {
		// Only old ones: another process may be mid-write right now.
		if fi, err := os.Stat(name); err == nil && fi.ModTime().Before(cutoff) {
			_ = os.Remove(name)
		}
	}
}

func (f *FileBackend) Save(blob []byte) error { return writeAtomic(f.path(), blob) }

func (f *FileBackend) Lock() (func(), error) {
	h, err := lockFile(filepath.Join(f.Dir, ".lock"))
	if err != nil {
		return nil, err
	}
	return func() { _ = h.Close() }, nil
}

func (f *FileBackend) HomeRoot() string { return filepath.Join(f.Dir, "homes") }

// writeAtomic lands a file at 0600 via a private temp file and a rename, so
// a crash mid-write can never leave a truncated store behind.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	serr := f.Sync()
	cerr := f.Close()
	if err = errors.Join(werr, serr, cerr); err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) // best effort; the real file is untouched
		return err
	}
	// The rename itself must reach the disk. Without this a power loss can
	// leave the previous file in place — and its refresh tokens are the
	// ones that have just been spent.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
