//go:build linux || darwin

package hold

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// witnessed plants a record for testID, its meta.json last touched an hour
// ago, and says the session's directory.
func witnessed(t *testing.T, exit *int) (terminals, dir string, touched time.Time) {
	t.Helper()
	terminals = filepath.Join(t.TempDir(), "terminals")
	dir = holdwire.Dir(terminals, testID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := holdwire.Meta{ID: testID, Kind: holdwire.KindShell, Started: time.Now().Add(-2 * time.Hour), Exit: exit}
	if exit != nil {
		m.Ended = time.Now().Add(-time.Hour)
	}
	if err := holdwire.WriteMeta(dir, &m); err != nil {
		t.Fatal(err)
	}
	touched = time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(holdwire.MetaPath(dir), touched, touched); err != nil {
		t.Fatal(err)
	}
	return terminals, dir, touched
}

// watchUntilGone runs a witness on dir and has the holder go: its end of
// the pipe closes, as it does however the holder ends. It says when.
func watchUntilGone(t *testing.T, dir string) time.Time {
	t.Helper()
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		witness(r, testID, dir)
	}()
	time.Sleep(20 * time.Millisecond) // the witness is waiting, not yet told
	gone := time.Now()
	w.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the witness never finished")
	}
	return gone
}

func mtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// TestAWitnessDatesAHolderThatWentWithoutAnEnding: a holder killed outright
// records nothing, and with no daemon to see it go its witness writes the
// moment into its record, which every reader takes for when it stopped.
func TestAWitnessDatesAHolderThatWentWithoutAnEnding(t *testing.T) {
	terminals, dir, _ := witnessed(t, nil)
	gone := watchUntilGone(t, dir)
	at := mtime(t, holdwire.MetaPath(dir))
	if at.Before(gone) || at.After(time.Now()) {
		t.Fatalf("the record says %v; the holder went at %v", at, gone)
	}
	r := holdwire.Judge(terminals, testID)
	if r.State != holdwire.StateInterrupted || !r.Stopped.Equal(at) {
		t.Fatalf("the record reads %s, stopped at %v, not %v", r.State, r.Stopped, at)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the witness left files: %v", entries)
	}
}

// TestAWitnessLeavesWhatIsAlreadyKnown: an ending the holder recorded is
// its own, and a daemon running sees the holder go itself and writes that
// moment, which is what it says before and after it stops keeping the
// session; a record that is not there is never made.
func TestAWitnessLeavesWhatIsAlreadyKnown(t *testing.T) {
	t.Run("an ending recorded", func(t *testing.T) {
		code := 0
		_, dir, touched := witnessed(t, &code)
		watchUntilGone(t, dir)
		if at := mtime(t, holdwire.MetaPath(dir)); !at.Equal(touched) {
			t.Fatalf("the record was touched at %v", at)
		}
	})
	t.Run("a daemon running", func(t *testing.T) {
		terminals, dir, touched := witnessed(t, nil)
		claim(t, terminals)
		watchUntilGone(t, dir)
		if at := mtime(t, holdwire.MetaPath(dir)); !at.Equal(touched) {
			t.Fatalf("the record was touched at %v", at)
		}
	})
	t.Run("no record", func(t *testing.T) {
		_, dir, _ := witnessed(t, nil)
		if err := os.Remove(holdwire.MetaPath(dir)); err != nil {
			t.Fatal(err)
		}
		watchUntilGone(t, dir)
		if _, err := os.Lstat(holdwire.MetaPath(dir)); !os.IsNotExist(err) {
			t.Fatalf("a record was made: %v", err)
		}
	})
	t.Run("another session's record", func(t *testing.T) {
		_, dir, touched := witnessed(t, nil)
		r, w := io.Pipe()
		w.Close()
		witness(r, "fedcba9876543210", dir)
		if at := mtime(t, holdwire.MetaPath(dir)); !at.Equal(touched) {
			t.Fatalf("the record was touched at %v", at)
		}
	})
}

// TestARunningHolderTouchesItsRecordEveryMinute: what a holder that went
// with its witness — a reboot, the power, both killed at once — is dated
// by is the last time it touched its record, so it touches it at least
// every minute while the CLI runs.
func TestARunningHolderTouchesItsRecordEveryMinute(t *testing.T) {
	if touchEvery > time.Minute {
		t.Fatalf("the record is touched every %v", touchEvery)
	}
	old := touchEvery
	touchEvery = 20 * time.Millisecond
	t.Cleanup(func() { touchEvery = old })

	c := newFakeChild()
	r := newRig(t, 0, c)
	dir := holdwire.Dir(r.terminals, testID)
	if err := holdwire.WriteMeta(dir, &holdwire.Meta{ID: testID, Kind: holdwire.KindShell}); err != nil {
		t.Fatal(err)
	}
	long := time.Now().Add(-time.Hour)
	if err := os.Chtimes(holdwire.MetaPath(dir), long, long); err != nil {
		t.Fatal(err)
	}
	h := &holder{sess: r.s, rec: &metaFile{dir: dir}, log: func(string, ...any) {}}
	kept := make(chan struct{})
	go func() {
		defer close(kept)
		h.keep()
	}()
	for deadline := time.Now().Add(5 * time.Second); mtime(t, holdwire.MetaPath(dir)).Before(time.Now().Add(-time.Second)); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the record was never touched")
		}
	}
	c.exit(0)
	select {
	case <-kept:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder kept the record after its CLI ended")
	}
}
