//go:build unix

package notify

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a secret file should be, or a link to one, is refused at
// once: opening it the plain way would wait for a writer, and with it the
// daemon's start.
func TestASecretFileThatIsAFIFOIsRefusedAtOnce(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "tg.token")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.token")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, link} {
		done := make(chan error, 1)
		go func() {
			_, err := readSecretFile("bot_token_file", path, "bot token", 4096)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "bot_token_file "+path+" is not a plain file") {
				t.Fatalf("%s: %v", path, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("reading %s waited for a writer", path)
		}
	}
}
