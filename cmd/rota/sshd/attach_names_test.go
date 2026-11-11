//go:build linux || darwin

package sshd

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/share"
)

/* ---------------------------------------- what a session is called, and by --- */

// The way back that the last line gives is typed as it is, after `ssh -t
// host`: a name a shell would have to quote, whose quotes the person's shell
// takes off before ssh joins the words with spaces, is given as the id.
func TestTheWayBackNeedsNoQuoting(t *testing.T) {
	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	for _, x := range []struct{ label, line string }{
		{"Fix login flow", "attach 'Fix login flow'"},
		{"it's", `attach "it's"`},
	} {
		s := attachHold(t, r, x.label, 80, 24)
		a := attachOpen(t, c, x.line, 80, 24)
		a.wait(0, "ready 80x24")
		a.typ("\x1cd")
		if code := a.end(); code != 0 {
			t.Fatalf("%s: exit %d; stderr %q", x.line, code, a.err.String())
		}
		back := "attach " + s.ID
		if got, want := a.err.String(), "rota: detached from "+x.label+"; "+back+" returns to it\r\n"; got != want {
			t.Errorf("stderr %q, want %q", got, want)
		}
		// And the way back is the way back.
		a = attachOpen(t, c, back, 80, 24)
		a.wait(0, "ready 80x24")
		a.typ("\x1cd")
		if code := a.end(); code != 0 || !strings.Contains(a.err.String(), "detached from "+x.label+";") {
			t.Errorf("%s: %d %q", back, code, a.err.String())
		}
	}
	attachGivenBack(t, r)
}

// A plain word is one a shell leaves as it is and words reads back whole.
func TestAWayBackIsPlain(t *testing.T) {
	for _, w := range []string{"api", "web-2.0_x@y:z,w/v%u+t=s", "0123456789abcdef", "a=b"} {
		if !attachPlain(w) {
			t.Errorf("%q is not plain", w)
		}
	}
	// =api is zsh's path of the command api.
	for _, w := range []string{"", "=api", "my api", "it's", `say"hi"`, `a\b`, "$HOME", "a;b", "a|b", "a&b", "*", "~x",
		"a`b`", "(x)", "<x>", "a#b", "a!b", "a?b", "[x]", "{x}", "a\tb", "a\nb", "café", "\x1b[2J"} {
		if attachPlain(w) {
			t.Errorf("%q is plain", w)
		}
	}
}

// A name holding escape sequences, a bell or a C1 control reaches no line
// rota writes as it is: the crop's note, the line an attach ends with and
// the line saying the session has ended each show it cleaned.
func TestAHostileNameIsCleanedOnEveryLine(t *testing.T) {
	r := commandsStart(t, commandsTty)
	const evil, clean = "ev\x1b]0;pwn\x07il\u009b31m\x1b[2J", "ev ]0;pwn il 31m [2J"
	s, err := r.c.Create(client.Create{Account: 1, Label: evil, Cwd: r.work, Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	raw := func(where, s string) {
		t.Helper()
		if strings.Contains(s, "\x1b]0;pwn") || strings.ContainsAny(s, "\x07\u009b") {
			t.Errorf("the name reached %s as it is: %q", where, attachTail(s))
		}
	}
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach "+s.ID, 60, 12)
	a.wait(0, "z fits; "+clean+" is 80x24")
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, a.err.String())
	}
	raw("the screen", a.out.String())
	if got, want := a.err.String(), "rota: detached from "+clean+"; attach "+s.ID+" returns to it\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}

	if err := r.c.Stop(s.ID); err != nil {
		t.Fatal(err)
	}
	sshdEventually(t, func() error {
		got, err := r.c.Get(s.ID)
		if err == nil && got.Ended == nil {
			err = errors.New("still running")
		}
		return err
	})
	out, errOut, code := commandsPty(t, c, "attach "+s.ID, 80, 24)
	if code != 1 || errOut != "session "+clean+" has ended\r\n" {
		t.Errorf("the ended line: %d %q", code, errOut)
	}
	raw("stdout", out)
	attachGivenBack(t, r)
}

// A name finds a terminal this server started, an account's CLI or a shell,
// and never one a process on this machine shared, whose label that process
// chose: a shared one is found by its id alone. An id is never taken by a
// label that spells it, and two live sessions of one name are refused,
// never guessed between.
func TestANameFindsOnlyWhatThisServerStarted(t *testing.T) {
	const id = "5ba7ed0000000001"
	// A held session says its state, whatever its kind; a terminal the
	// server runs itself says none, and is named only for the kinds it
	// starts.
	for _, x := range []struct {
		s     client.Session
		named bool
	}{
		{client.Session{State: "live"}, true},
		{client.Session{State: "exited", Kind: "later"}, true},
		{client.Session{Kind: "account"}, true},
		{client.Session{Kind: "shell"}, true},
		{client.Session{Kind: "shared"}, false},
		{client.Session{}, false},
	} {
		if got := attachNamed(x.s); got != x.named {
			t.Errorf("attachNamed(%+v) = %v", x.s, got)
		}
	}
	for _, x := range []struct {
		s     client.Session
		found bool
	}{
		{client.Session{ID: id, Kind: "account", Label: "api"}, true},
		{client.Session{ID: id, Kind: "shell", Label: "api"}, true},
		{client.Session{ID: id, Kind: "shared", Label: "api"}, false},
		{client.Session{ID: id, Kind: "shared", Cwd: "/w/api"}, false},
		{client.Session{ID: id, Kind: "", Label: "api"}, false},
		{client.Session{ID: id, Kind: "later", Label: "api"}, false},
	} {
		got, err := attachFind([]client.Session{x.s}, "api")
		switch {
		case x.found && (err != nil || got.ID != id):
			t.Errorf("%+v by its label: %v %+v", x.s, err, got)
		case !x.found && !errors.Is(err, client.ErrNoSession):
			t.Errorf("%+v was found by its label: %v %+v", x.s, err, got)
		}
		if got, err := attachFind([]client.Session{x.s}, id); err != nil || got.ID != id {
			t.Errorf("%+v by its id: %v %+v", x.s, err, got)
		}
	}

	r := commandsStart(t, commandsTty)
	c := r.signIn(r.me)
	api := r.open(t, "api")
	// A session labelled with api's id: the id is still api's.
	r.open(t, api.ID)
	a := attachOpen(t, c, "attach "+api.ID, 80, 24)
	a.wait(0, "ready 80x24")
	a.typ("\x1cd")
	if code := a.end(); code != 0 || a.err.String() != "rota: detached from api; attach api returns to it\r\n" {
		t.Errorf("attach %s: %d %q", api.ID, code, a.err.String())
	}
	// Two live sessions called api: neither is guessed.
	r.open(t, "api")
	for _, cmd := range []string{"attach", "watch"} {
		_, errOut, code := commandsPty(t, c, cmd+" api", 80, 24)
		if code != 1 || !strings.HasPrefix(errOut, `2 live sessions are called "api"`) || !strings.Contains(errOut, api.ID) {
			t.Errorf("%s api with two: %d %q", cmd, code, errOut)
		}
	}
	attachGivenBack(t, r)
}

// attachShare offers a terminal on the rig's share socket, as `rota run
// --share` would, labelled label, which says it is ready, again and again,
// for five seconds. It gives the terminal's id.
func attachShare(t *testing.T, label string) string {
	t.Helper()
	path := filepath.Join(os.Getenv("ROTA_HOME"), "terminals", share.SockName)
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := share.NewConn(conn, 0)
	t.Cleanup(func() { c.Close() })
	hello := share.HelloMsg{Version: share.Version, Account: 1, Label: label, Cwd: "/src/" + label, Cols: 80, Rows: 24, PID: 4242, Mode: share.ModeControl}
	if err := c.SendJSON(share.Hello, hello); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := c.Read()
	if err != nil || kind != share.Welcome {
		t.Fatalf("share hello: %d %q %v", kind, payload, err)
	}
	var yes share.WelcomeMsg
	if err := share.Unmarshal(payload, &yes); err != nil {
		t.Fatal(err)
	}
	c.Clear()
	go func() {
		for {
			if _, _, err := c.Read(); err != nil {
				return
			}
		}
	}()
	go func() {
		for range 50 {
			if c.Send(share.Output, []byte("shared-ready\r\n")) != nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return yes.Terminal
}

// A shared terminal is found by its id alone, so the way back its last line
// gives is its id, never the label its sharer chose: that label finds
// nothing, or, once a session of the server's has it, that session.
func TestASharedTerminalsWayBackIsItsID(t *testing.T) {
	r := commandsStart(t, commandsTty)
	id := attachShare(t, "api")
	c := r.signIn(r.me)
	if _, errOut, code := commandsPty(t, c, "attach api", 80, 24); code != 1 || !strings.HasPrefix(errOut, `no session is "api"`) {
		t.Errorf("attach api, shared only: %d %q", code, errOut)
	}
	back := "attach " + id
	for range 2 {
		// The way back is the way back.
		a := attachOpen(t, c, back, 80, 24)
		a.wait(0, "shared-ready")
		a.typ("\x1cd")
		if code := a.end(); code != 0 {
			t.Fatalf("%s: exit %d; stderr %q", back, code, a.err.String())
		}
		if got, want := a.err.String(), "rota: detached from api; "+back+" returns to it\r\n"; got != want {
			t.Errorf("stderr %q, want %q", got, want)
		}
	}
	// A session of the server's called api changes none of that.
	r.open(t, "api")
	a := attachOpen(t, c, back, 80, 24)
	a.wait(0, "shared-ready")
	a.typ("\x1cd")
	if code := a.end(); code != 0 || a.err.String() != "rota: detached from api; "+back+" returns to it\r\n" {
		t.Errorf("with a live api too: %d %q", code, a.err.String())
	}
	attachGivenBack(t, r)
}
