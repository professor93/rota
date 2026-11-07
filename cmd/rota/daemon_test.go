package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/wire"
)

// Foreground `rota serve` is the server it always was: it claims nothing of
// the daemon's, opens no control socket, and says on stderr, line for line,
// what it said before there was a daemon. The lines below were captured from
// the commit before serve start existed.
func TestForegroundServeIsUnchanged(t *testing.T) {
	var addr string
	_, stderr, code := serveFor(t, nil, func(base string) { addr = strings.TrimPrefix(base, "http://") })
	want := "rota " + wire.Version + " serving on http://" + addr + "\n" +
		"warning: no --root given, so a caller may name any directory on this machine\n" +
		"\n" +
		"rota: shutting down; waiting for runs in flight (interrupt again to stop now)\n"
	if code != 0 || stderr != want {
		t.Fatalf("foreground serve: exit %d, said\n%q\nwant\n%q", code, stderr, want)
	}
	terminals := filepath.Join(os.Getenv("ROTA_HOME"), "terminals")
	for _, name := range []string{"daemon.lock", "daemon.pid", "ctl.sock"} {
		if _, err := os.Lstat(filepath.Join(terminals, name)); !os.IsNotExist(err) {
			t.Fatalf("foreground serve left %s: %v", name, err)
		}
	}
}
