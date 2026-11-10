//go:build linux || darwin

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// daemonFits skips a test whose store has no room for a control socket.
func daemonFits(t *testing.T) {
	t.Helper()
	if err := holdwire.SockFits(holdwire.CtlSock(filepath.Join(os.Getenv("ROTA_HOME"), "terminals"))); err != nil {
		t.Skipf("no daemon fits here: %v", err)
	}
}

// A serve start whose binary cannot be run says so on one line, a newline
// in the binary's path drawn as a space, and the error is still the one
// exec gave, a file that is not there.
func TestAStartThatCannotRunItsBinarySaysSoOnOneLine(t *testing.T) {
	shortHome(t)
	daemonFits(t)
	missing := filepath.Join(t.TempDir(), "no\nsuch", "rota")
	was := daemonBinary
	daemonBinary = func() (string, error) { return missing, nil }
	t.Cleanup(func() { daemonBinary = was })

	out, errb, code := call(t, "serve", "start")
	want := "error: starting the rota daemon: fork/exec " + strings.ReplaceAll(missing, "\n", " ") + ": no such file or directory\n"
	if code != 1 || out != "" || errb != want {
		t.Fatalf("serve start: exit %d %q %q, want %q", code, out, errb, want)
	}
	c := &cli{out: &strings.Builder{}, err: &strings.Builder{}, in: strings.NewReader("")}
	if _, err := c.spawnDaemon(false, ""); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the error is no longer exec's: %v", err)
	}
}

// A daemon whose certificate cannot be read logs why on one line, with a
// newline in the certificate's path drawn as a space, rather than the
// escaped newline the log would show otherwise; serve start shows the
// daemon's log lines as they are written.
func TestADaemonWhoseCertificateCannotBeReadLogsItsPathCleaned(t *testing.T) {
	shortHome(t)
	daemonFits(t)
	home := os.Getenv("ROTA_HOME")
	dir := filepath.Join(t.TempDir(), "no\nsuch")
	conf := "[server]\nlisten = \"127.0.0.1:0\"\n[tls]\ncert = \"" + strings.ReplaceAll(dir, "\n", `\n`) + "/cert.pem\"\n" +
		"key = \"" + strings.ReplaceAll(dir, "\n", `\n`) + "/key.pem\"\n"
	if err := os.WriteFile(filepath.Join(home, "server.toml"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, errb, code := call(t, "serve", "start", "--foreground"); code != 1 || out != "" || errb != "" {
		t.Fatalf("serve start --foreground: exit %d %q %q", code, out, errb)
	}
	raw, _ := os.ReadFile(filepath.Join(home, "daemon.log"))
	if want := "tls: open " + strings.ReplaceAll(dir, "\n", " ") + "/cert.pem: no such file or directory"; !strings.Contains(string(raw), want) {
		t.Fatalf("the log does not say %q:\n%s", want, raw)
	}
}

// oneLineError draws its error as oneLine does, and errors.Is and errors.As
// still find the error it was.
func TestOneLineErrorKeepsTheErrorItWas(t *testing.T) {
	base := &fs.PathError{Op: "open", Path: "/a\nb\x1b[2J", Err: fs.ErrNotExist}
	err := oneLineError{base}
	if got, want := err.Error(), "open /a b [2J: file does not exist"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	var pe *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pe) || pe.Path != base.Path {
		t.Fatalf("errors.Is and errors.As lost the error: %v", err)
	}
}
