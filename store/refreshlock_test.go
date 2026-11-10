package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A lock rota made but could not open — no descriptor free, say — is no
// lock of rota's and nobody's: the take says so, never as a lock somebody
// holds, and leaves nothing at the name for Claude Code to wait on. Only
// rota's own directory is removed: one somebody made at the name meanwhile
// stays.
func TestALockMadeButNotOpenedIsNotLeftBehind(t *testing.T) {
	seam := func(t *testing.T, open func(path string, made fs.FileInfo) (lockDir, error)) {
		t.Helper()
		old := openTakenLock
		openTakenLock = open
		t.Cleanup(func() { openTakenLock = old })
	}
	t.Run("no descriptor free", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, claudeRefreshLock)
		full := errors.New("too many open files")
		seam(t, func(string, fs.FileInfo) (lockDir, error) { return lockDir{}, full })
		l, err := takeRefreshLock(home)
		if l != nil {
			t.Cleanup(l.release)
		}
		if !errors.Is(err, full) || errors.Is(err, errLockHeld) {
			t.Fatalf("the take says %v", err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the lock is left behind: %v", err)
		}
		if _, held := heldLocks.Load(path); held {
			t.Fatal("and is counted as held")
		}
	})
	t.Run("somebody else's at the name", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, claudeRefreshLock)
		var theirs fs.FileInfo
		seam(t, func(path string, made fs.FileInfo) (lockDir, error) {
			// Moved aside, not removed, so that theirs cannot be given the
			// inode of rota's.
			if err := os.Rename(path, path+".rota"); err != nil {
				t.Error(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Error(err)
			}
			var err error
			if theirs, err = os.Lstat(path); err != nil {
				t.Error(err)
			}
			return openLockDir(path, made)
		})
		l, err := takeRefreshLock(home)
		if l != nil {
			// Here openLockDir goes by the name and opens nothing; the beat
			// tells theirs from rota's, and the release leaves it.
			l.release()
		}
		if unix := runtime.GOOS == "linux" || runtime.GOOS == "darwin"; unix && (err == nil || errors.Is(err, errLockHeld)) {
			t.Fatalf("the take says %v", err)
		}
		if fi, err := os.Lstat(path); err != nil || !os.SameFile(fi, theirs) {
			t.Fatalf("theirs is removed: %v", err)
		}
		if _, held := heldLocks.Load(path); held {
			t.Fatal("and is counted as rota's")
		}
	})
}
