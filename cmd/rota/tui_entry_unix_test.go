//go:build linux || darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/cmd/rota/tui"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
)

// The TUI opens only where both ends are a terminal it can draw on: a
// pseudo-terminal at both is one, a pipe at either is not, and neither is a
// terminal that says it is dumb.
func TestATerminalIsBothEnds(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	t.Setenv("TERM", "xterm-256color")
	if !tuiTerminal(slave, slave) {
		t.Fatal("a terminal at both ends was not taken for one")
	}
	if tuiTerminal(slave, w) {
		t.Fatal("a terminal in and a pipe out was taken for a terminal")
	}
	if tuiTerminal(r, slave) {
		t.Fatal("a pipe in and a terminal out was taken for a terminal")
	}
	if tuiTerminal(strings.NewReader(""), slave) {
		t.Fatal("a reader that is no file was taken for a terminal")
	}
	t.Setenv("TERM", "dumb")
	if tuiTerminal(slave, slave) {
		t.Fatal("a dumb terminal was taken for one the TUI can draw on")
	}
}

// tuiRecord writes a held session's record under terminals, as its holder
// would, and returns its directory.
func tuiRecord(t *testing.T, terminals, id string, m holdwire.Meta) string {
	t.Helper()
	dir := holdwire.Dir(terminals, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m.ID, m.Protocol, m.Started = id, holdwire.Majors[0], time.Now()
	if err := holdwire.WriteMeta(dir, &m); err != nil {
		t.Fatal(err)
	}
	return dir
}

// With the daemon running, the header's answer is the daemon's: its pid and
// the sessions it holds, asked over its socket, and not one that has
// stopped, which the daemon still lists; a call ends when its context does.
func TestTheDaemonIsAskedWhenItRuns(t *testing.T) {
	home, terminals := ctlStore(t)
	code := 0
	tuiRecord(t, terminals, "00000000000000b2", holdwire.Meta{Name: "done", Account: 1, Exit: &code})
	srv, err := api.New(api.Options{Token: "x", Dir: home, RefreshEvery: -1,
		HoldCommand: func() *exec.Cmd { return exec.Command(os.DevNull) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	stop, err := listenCtl(srv, terminals, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	cl, err := rotaclient.DialNow(terminals)
	if err != nil {
		t.Fatal(err)
	}
	if listed, err := cl.List(); err != nil || len(listed) != 1 || listed[0].Ended == nil {
		t.Fatalf("the daemon lists %+v %v; want the stopped session", listed, err)
	}
	daemon := localDaemon(terminals)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if d := daemon(ctx); !d.Running || d.PID != os.Getpid() || d.Err != "" || len(d.Live) != 0 {
		t.Fatalf("a daemon holding nothing live: %+v", d)
	}
	ended, end := context.WithCancel(context.Background())
	end()
	began := time.Now()
	if d := daemon(ended); !d.Running || d.Err == "" {
		t.Fatalf("a call whose context had ended: %+v; want running, with why it could not ask", d)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("a call whose context had ended took %v", took)
	}
}

// With no daemon, a holder that outlived the one that started it still
// holds its session, which the header lists, while a session that ended is
// not; and no daemon is said to run.
func TestHoldersOutlivingTheDaemonAreListed(t *testing.T) {
	_, terminals := ctlStore(t)
	release, err := holdwire.Lock(tuiRecord(t, terminals, "00000000000000a1", holdwire.Meta{Name: "api", Account: 2}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	code := 0
	tuiRecord(t, terminals, "00000000000000a2", holdwire.Meta{Name: "done", Account: 1, Exit: &code})
	d := localDaemon(terminals)(context.Background())
	want := []tui.Live{{ID: "00000000000000a1", Name: "api", Account: 2}}
	if d.Running || d.Err != "" || len(d.Live) != 1 || d.Live[0] != want[0] {
		t.Fatalf("no daemon, one holder left: %+v; want %+v", d, want)
	}
}

// A session the daemon lists is live until it has ended.
func TestOnlyLiveSessionsAreTheHeaders(t *testing.T) {
	now := time.Now()
	got := liveSessions([]rotaclient.Session{
		{ID: "00000000000000c1", Name: "api", Account: 2},
		{ID: "00000000000000c2", Name: "done", Account: 1, Ended: &now},
		{ID: "00000000000000c3", Name: "shell"},
	})
	want := []tui.Live{{ID: "00000000000000c1", Name: "api", Account: 2}, {ID: "00000000000000c3", Name: "shell"}}
	if !slices.Equal(got, want) {
		t.Fatalf("live: %+v; want %+v", got, want)
	}
}
