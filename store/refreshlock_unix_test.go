//go:build linux || darwin

package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Whatever is put in the lock's place while rota holds it — a FIFO, which
// an open would block on until a writer came, or a link — is not rota's: the
// next beat stands down without opening it, the release returns at once,
// and it is left where it is.
func TestWhatTakesTheLocksPlaceNeverHoldsUpItsRelease(t *testing.T) {
	for _, c := range []struct {
		what string
		put  func(t *testing.T, path string)
	}{
		{"a FIFO", func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a link to a directory", func(t *testing.T, path string) {
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			old := refreshLockBeat
			refreshLockBeat = 10 * time.Millisecond
			t.Cleanup(func() { refreshLockBeat = old })
			stoodDown := make(chan struct{})
			var once sync.Once
			refreshLockLooked = func(ours bool) {
				if !ours {
					once.Do(func() { close(stoodDown) })
				}
			}
			t.Cleanup(func() { refreshLockLooked = nil })
			home := t.TempDir()
			l, err := takeRefreshLock(home)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(l.release) // its beat must not outlive a test that failed
			path := filepath.Join(home, claudeRefreshLock)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			c.put(t, path)
			select {
			case <-stoodDown:
			case <-time.After(30 * time.Second):
				t.Fatal("no beat looked at the lock again")
			}
			released := make(chan struct{})
			go func() { l.release(); close(released) }()
			select {
			case <-released:
			case <-time.After(30 * time.Second):
				t.Fatal("the release never returned")
			}
			if fi, err := os.Lstat(path); err != nil || fi.IsDir() {
				t.Fatalf("what took the lock's place stays: %v %v", fi, err)
			}
		})
	}
}

// A FIFO or a link at the lock's name the moment rota has made it is no
// lock of rota's: the open fails at once, without blocking.
func TestOnlyTheDirectoryRotaMadeIsOpened(t *testing.T) {
	dir := t.TempDir()
	made := filepath.Join(dir, "made")
	if err := os.Mkdir(made, 0o700); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(made)
	if err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(made, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link, other} {
		opened := make(chan error, 1)
		go func() {
			d, err := openLockDir(path, fi)
			if err == nil {
				d.close()
			}
			opened <- err
		}()
		select {
		case err := <-opened:
			if err == nil {
				t.Errorf("%s was opened as the lock rota made", filepath.Base(path))
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("opening %s blocked", filepath.Base(path))
		}
	}
	d, err := openLockDir(made, fi)
	if err != nil {
		t.Fatal(err)
	}
	d.close()
}

// The directory rota held open while it held the lock is closed once it lets
// go: one left open for every lock would run a long-lived rota out of
// descriptors, and then no lock could be taken and nothing refreshed.
func TestLettingGoOfTheLockClosesItsDirectory(t *testing.T) {
	l, err := takeRefreshLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.release)
	l.release()
	if _, err := l.dir.f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the lock's directory is still open after the release: %v", err)
	}
}
