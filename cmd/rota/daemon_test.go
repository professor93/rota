package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/professor93/rota/api"
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

// --yes answers the question --all asks; given alone it is a mistake worth
// saying, before anything is stopped.
func TestServeStopYesNeedsAll(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	if out, errb, code := call(t, "serve", "stop", "--yes"); code != 2 || out != "" || !strings.Contains(errb, "--yes goes with --all") {
		t.Fatalf("serve stop --yes: exit %d %q %q", code, out, errb)
	}
}

// A daemon has no flags for what its file says, so the notes it logs name
// the file's keys; foreground serve's name its flags, as they always did.
func TestADaemonsNotesNameItsKeys(t *testing.T) {
	opts := &api.Options{AllowDangerous: true, AllowRawFlags: true, Routes: &api.Routes{API: true, Playground: true, WebSocket: true, Health: true}}
	at := api.Listener{Addr: "127.0.0.1:8787"}
	daemon := strings.Join(serveNotes(opts, at, true), "\n")
	if strings.Contains(daemon, "--") || !strings.Contains(daemon, "no runs.roots set") ||
		!strings.Contains(daemon, "runs.allow_dangerous is on") || !strings.Contains(daemon, "runs.allow_raw_flags is on") {
		t.Fatalf("a daemon's notes:\n%s", daemon)
	}
	foreground := strings.Join(serveNotes(opts, at, false), "\n")
	for _, want := range []string{
		"warning: no --root given, so a caller may name any directory on this machine",
		"warning: --allow-dangerous is on; callers may bypass every permission check",
		"warning: --allow-raw-flags is on; a caller can pass any vendor flag, which undoes --root and --allow-dangerous",
	} {
		if !strings.Contains(foreground, want) {
			t.Fatalf("foreground serve's notes lost %q:\n%s", want, foreground)
		}
	}
}

// What the daemon's verbs print for -h fits an 80-column terminal, tabs
// counted to the next multiple of eight as a terminal draws them.
func TestTheDaemonsUsageFitsEightyColumns(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, verb := range []string{"start", "stop", "status"} {
		out, _, code := call(t, "serve", verb, "-h")
		if code != 0 || !strings.Contains(out, "rota serve status") {
			t.Fatalf("serve %s -h: exit %d %q", verb, code, out)
		}
		for _, line := range strings.Split(out, "\n") {
			col := 0
			for _, r := range line {
				if r == '\t' {
					col += 8 - col%8
				} else {
					col++
				}
			}
			if col > 80 {
				t.Errorf("serve %s -h has a line of %d columns: %q", verb, col, line)
			}
		}
	}
}
