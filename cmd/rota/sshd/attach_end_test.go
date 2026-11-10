//go:build linux || darwin

package sshd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/fakecli"
)

/* ------------------------------------------- how an attach over SSH ends --- */

func TestTheEndOfASessionEndsTheChannel(t *testing.T) {
	r := commandsStart(t, fakecli.Spec{Tty: true, Exit: 3})
	s := r.open(t, "api")
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	a.typ("bye\r")
	if code := a.end(); code != 3 {
		t.Fatalf("the CLI's exit 3 ended the channel with %d; stderr %q", code, a.err.String())
	}
	if got, want := a.err.String(), "rota: api ended, exit 3\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if out := a.out.String(); !strings.HasSuffix(out, attachReset(24)) {
		t.Errorf("the screen does not end with the reset: %q", attachTail(out))
	}
	if got, err := r.c.Get(s.ID); err != nil || got.Ended == nil {
		t.Errorf("the session is not ended: %v %+v", err, got)
	}
	attachGivenBack(t, r)
}

func TestAnAttachTakesAPlace(t *testing.T) {
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	c := r.signIn(r.me)
	seats := []*ssh.Session{limitsSeat(t, r.sshdRig, c), limitsSeat(t, r.sshdRig, c)}
	const full = "rota over ssh has 2 sessions attached on this connection; leave one first\r\n"
	for _, line := range []string{"attach api", "watch api", "new --label z"} {
		if _, errOut, code := commandsPty(t, c, line, 80, 24); code != 1 || errOut != full {
			t.Errorf("%s with no place: %d %q", line, code, errOut)
		}
	}
	if got := r.created(); len(got) != 0 {
		t.Errorf("new with no place asked for %q", got)
	}
	seats[0].Close()
	sshdEventually(t, func() error {
		if n := attachPlaces(r); n != 1 {
			return fmt.Errorf("%d places", n)
		}
		return nil
	})
	a := attachOpen(t, c, "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	if n := attachPlaces(r); n != 2 {
		t.Errorf("%d places held, want 2", n)
	}
	a.typ("\x1cd")
	if code := a.end(); code != 0 {
		t.Fatalf("exit %d; stderr %q", code, a.err.String())
	}
	sshdEventually(t, func() error {
		if n := attachPlaces(r); n != 1 {
			return fmt.Errorf("%d places", n)
		}
		return nil
	})
	seats[1].Close()
	attachGivenBack(t, r)
}

func TestAPanicGivesTheTerminalBack(t *testing.T) {
	// Set before the server starts and put back once it has stopped, after
	// every goroutine of it has returned: what reads it is the server's.
	old := attachLoop
	t.Cleanup(func() { attachLoop = old })
	attachLoop = func(_ context.Context, t attach.Terminal, _ *client.Client, _ string, _ attach.Options) attach.Result {
		_, _ = t.Write([]byte("\x1b[?1049h\x1b[?1000h\x1b[?25l"))
		panic("boom")
	}
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach api", 80, 24)
	select {
	case err := <-a.done:
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			t.Errorf("a panicking attach ended with exit %d, not the connection", exit.ExitStatus())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panicking attach never ended")
	}
	if out := a.out.String(); !strings.HasSuffix(out, "\x1b[?25l"+attachReset(24)) {
		t.Errorf("the terminal was not given back: %q", out)
	}
	attachGivenBack(t, r)
	if !strings.Contains(r.log.String(), "ssh panic") {
		t.Errorf("the panic was not logged:\n%s", r.log)
	}
	if out, _, code := sshdRun(t, r.signIn(r.me), "list"); code != 0 || !strings.Contains(out, "  api  ") {
		t.Errorf("the server stopped serving: %d %q", code, out)
	}
}

func TestAnAttachEndsWithItsConnection(t *testing.T) {
	r := commandsStart(t, commandsTty)
	s := r.open(t, "api")
	c := r.signIn(r.me)
	a := attachOpen(t, c, "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	c.Close()
	attachGivenBack(t, r)
	if got, err := r.c.Get(s.ID); err != nil || got.Ended != nil {
		t.Errorf("the session did not go on: %v %+v", err, got)
	}

	a = attachOpen(t, r.signIn(r.me), "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	began := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("Close took %s with an attach open", took)
	}
	attachGivenBack(t, r)
}
