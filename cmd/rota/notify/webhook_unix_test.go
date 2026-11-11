//go:build unix

package notify

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/api"
)

// A webhook's secret_file is read as every sink's secret file is: a FIFO
// there is refused at once, not waited on with the daemon's start; a link
// to a file others can read is refused by the link's name; a link to a file
// of its owner's is followed, and its key signs.
func TestAWebhookSecretFileIsAPlainFileOfItsOwners(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "hook.secret")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := newWebhook(api.EventSink{Type: "webhook", URL: "https://hooks.example/h", SecretFile: fifo}, env{})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "secret_file "+fifo+" is not a plain file" {
			t.Fatalf("a FIFO secret_file gave %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a FIFO secret_file waited for a writer")
	}

	open := hookKeyFile(t, "OPENKEY-123")
	if err := os.Chmod(open, 0o644); err != nil {
		t.Fatal(err)
	}
	openLink := filepath.Join(t.TempDir(), "hook.secret")
	if err := os.Symlink(open, openLink); err != nil {
		t.Fatal(err)
	}
	s, err := newWebhook(api.EventSink{Type: "webhook", URL: "https://hooks.example/h", SecretFile: openLink}, env{})
	if s != nil || err == nil || !strings.Contains(err.Error(), "chmod 600 "+openLink) {
		t.Fatalf("a link to a secret file others can read gave %v, %v", s, err)
	}
	if strings.Contains(err.Error(), "OPENKEY") {
		t.Fatalf("%v quotes the key", err)
	}

	link := filepath.Join(t.TempDir(), "hook.secret")
	if err := os.Symlink(hookKeyFile(t, " LINKKEY\n"), link); err != nil {
		t.Fatal(err)
	}
	w := hookNew(t, "https://hooks.example/h", link)
	if string(w.key) != "LINKKEY" {
		t.Fatalf("a link to an owner's secret file gave the key %q", w.key)
	}
}
