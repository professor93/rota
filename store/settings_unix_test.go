//go:build unix

package store

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a settings file should be is refused at once and never waited
// on, whether it was there when the path was looked at or put there between
// that look and the open: an open that waited for a writer would hold the
// launch until somebody wrote, which may be never. One somebody has written
// into is not read either: it is not a regular file, whatever it holds.
func TestAFIFOInPlaceOfASettingsFileIsNotWaitedOn(t *testing.T) {
	for _, c := range []struct {
		what    string
		read    func(path string, limit int64) ([]byte, bool)
		written bool
	}{
		{"there at the look", readSmallFile, false},
		{"put there after the look", openSmallFile, false},
		{"put there after the look, written into", openSmallFile, true},
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if c.written {
			// A reader of its own keeps what is written there for the next
			// open, after the writer has gone.
			keep, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer keep.Close()
			w, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.WriteString(`{"apiKeyHelper":"x"}`); err != nil {
				t.Fatal(err)
			}
			w.Close()
		}
		done := make(chan bool, 1)
		go func() {
			_, ok := c.read(path, maxSettingsFile)
			done <- ok
		}()
		select {
		case ok := <-done:
			if ok {
				t.Fatalf("%s: a FIFO is not a settings file", c.what)
			}
		case <-time.After(10 * time.Second):
			// A writer lets the open that is waiting go, so the test ends.
			if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				w.Close()
			}
			t.Fatalf("%s: the open waited for a writer", c.what)
		}
	}
}
