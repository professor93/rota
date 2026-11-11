//go:build linux || darwin

package sshd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
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

// Each attach takes one of the places the server keeps for them, once it
// has found the session and before it starts, and gives it back when it
// ends; new on a terminal takes it before it starts anything, and a sign-in
// that may only watch is refused for that before it takes one.
func TestAnAttachTakesAPlace(t *testing.T) {
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	old := r.open(t, "old")
	if err := r.c.Stop(old.ID); err != nil {
		t.Fatal(err)
	}
	sshdEventually(t, func() error {
		got, err := r.c.Get(old.ID)
		if err == nil && got.Ended == nil {
			err = errors.New("still running")
		}
		return err
	})
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
	// With every place taken, a session not found, or ended, is still said
	// as such: the place is asked for only once there is one to attach to.
	for _, cmd := range []string{"attach", "watch"} {
		if _, errOut, code := commandsPty(t, c, cmd+" nosuch", 80, 24); code != 1 || !strings.HasPrefix(errOut, `no session is "nosuch"`) {
			t.Errorf("%s nosuch with no place: %d %q", cmd, code, errOut)
		}
		if _, errOut, code := commandsPty(t, c, cmd+" "+old.ID, 80, 24); code != 1 || errOut != "session old has ended\r\n" {
			t.Errorf("%s of an ended session with no place: %d %q", cmd, code, errOut)
		}
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

	// eye, with every place of its connection taken, is told what its
	// sign-in may do, not that there is no place.
	ce := r.signIn(r.eye)
	limitsSeat(t, r.sshdRig, ce)
	limitsSeat(t, r.sshdRig, ce)
	if _, errOut, code := commandsPty(t, ce, "new --label w", 80, 24); code != 1 || errOut != "this sign-in may only watch; nothing here can be changed from it\r\n" {
		t.Errorf("eye's new on a terminal: %d %q", code, errOut)
	}
	all, err := r.c.List()
	if err != nil || len(all) != 2 {
		t.Errorf("eye's new started something: %v %+v", err, all)
	}
}

// attachRunning counts the goroutines now, and those of an attach over
// SSH: the window's follower, the channel's reader, and the attach's own.
func attachRunning() (all, ours int) {
	buf := make([]byte, 8<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		all++
		if strings.Contains(g, "sshd.(*chanTerm).follow") || strings.Contains(g, "sshd.(*input).pump") ||
			strings.Contains(g, "rota/cmd/rota/attach.") {
			ours++
		}
	}
	return all, ours
}

// Twenty attaches, one after another on one connection, leave nothing
// running once each has ended: no follower of a window, no reader of a
// channel, nothing of an attach.
func TestAttachesLeaveNothingRunning(t *testing.T) {
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	c := r.signIn(r.me)
	once := func() {
		t.Helper()
		a := attachOpen(t, c, "attach api", 80, 24)
		a.wait(0, "ready 80x24")
		a.typ("\x1cd")
		if code := a.end(); code != 0 {
			t.Fatalf("exit %d; stderr %q", code, a.err.String())
		}
	}
	once()
	attachGivenBack(t, r)
	// The baseline: once the first attach's goroutines have ended, or as
	// many of them as will.
	all0, ours0 := attachRunning()
	for deadline := time.Now().Add(5 * time.Second); ours0 > 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		all0, ours0 = attachRunning()
	}
	for range 20 {
		once()
	}
	attachGivenBack(t, r)
	all, ours := attachRunning()
	for deadline := time.Now().Add(5 * time.Second); (ours > ours0 || all > all0+2) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		all, ours = attachRunning()
	}
	if ours > ours0 || all > all0+2 {
		buf := make([]byte, 8<<20)
		t.Errorf("after 20 attaches %d goroutines (%d an attach's), from %d (%d):\n%s",
			all, ours, all0, ours0, buf[:runtime.Stack(buf, true)])
	}
}

// A panic in an attach gives the person's terminal back before it ends the
// connection, gives the attach's place back, and leaves the server serving.
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

// A connection that drops ends its attach, which gives its place back and
// leaves the session running; a server that stops ends every attach at once.
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

// A server that stops tells each attach first: it ends lost, says why on
// its channel with its exit-status, and gives the terminal back, and only
// then does the connection go. The session goes on.
func TestAStoppingServerSaysWhyToEachAttach(t *testing.T) {
	r := commandsStart(t, commandsTty)
	s := r.open(t, "api")
	a := attachOpen(t, r.signIn(r.me), "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	began := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("Close took %s with an attach open", took)
	}
	if code := a.end(); code != 1 {
		t.Errorf("a stopping server's attach exited %d, want 1", code)
	}
	if got, want := a.err.String(), "rota: lost api: the SSH server is stopping; attach api tries again\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
	if out := a.out.String(); !strings.HasSuffix(out, attachReset(24)) {
		t.Errorf("the screen does not end with the reset and a newline: %q", attachTail(out))
	}
	if got, err := r.c.Get(s.ID); err != nil || got.Ended != nil {
		t.Errorf("the session did not go on: %v %+v", err, got)
	}
	attachGivenBack(t, r)
}

// A command that does not end when it is told is not waited for beyond
// sshdCloseWait: the connection is cut under it, with no exit-status. Close
// itself returns only once the command has.
func TestCloseWaitsOnlySoLongForACommand(t *testing.T) {
	sshdKnob(t, &sshdCloseWait, 200*time.Millisecond)
	stuck := make(chan struct{})
	commands["stuck"] = func(context.Context, *Channel, []string) int {
		<-stuck
		return 0
	}
	t.Cleanup(func() { delete(commands, "stuck") })
	r := sshdStart(t)
	// Let go before the rig's own Close, which runs after this, so a failure
	// below ends the test rather than hanging it.
	var once sync.Once
	release := func() { once.Do(func() { close(stuck) }) }
	t.Cleanup(release)
	sess, err := r.signIn(r.me).NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Start("stuck"); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	closed := make(chan error, 1)
	go func() { closed <- r.s.Close() }()
	select {
	case err := <-waited:
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			t.Errorf("a command that never ended said exit %d", exit.ExitStatus())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client never saw its channel end")
	}
	release()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

// On a slow link a stopping server waits for each client to close its
// channel, which says it has the attach's last line and its exit-status,
// before it cuts the connection: a cut before then can lose both, as a
// reset loses what a socket had yet to deliver.
func TestAStoppingServerWaitsForTheClientOnASlowLink(t *testing.T) {
	r := commandsStart(t, commandsTty)
	r.open(t, "api")
	addr := slowLink(t, r.s.Addr(), 20*time.Millisecond)
	nc, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cc, chans, reqs, err := ssh.NewClientConn(nc, addr, r.config("me", ssh.PublicKeys(r.me)))
	if err != nil {
		t.Fatal(err)
	}
	c := ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { c.Close() })
	a := attachOpen(t, c, "attach api", 80, 24)
	a.wait(0, "ready 80x24")
	began := time.Now()
	if err := r.s.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > sshdCloseWait+time.Second {
		t.Errorf("Close took %s", took)
	}
	if code := a.end(); code != 1 {
		t.Errorf("an attach over a slow link to a stopping server exited %d, want 1", code)
	}
	if got, want := a.err.String(), "rota: lost api: the SSH server is stopping; attach api tries again\r\n"; got != want {
		t.Errorf("stderr %q, want %q", got, want)
	}
}

// slowLink is a TCP proxy to addr that passes every byte on delay after it
// came, each way, as a mobile link does. When either side goes, the other is
// closed at once and whatever the link still held for it is lost.
func slowLink(t *testing.T, addr string, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", addr)
			if err != nil {
				in.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, in, out)
			mu.Unlock()
			wg.Add(2)
			go func() { defer wg.Done(); slowPipe(in, out, delay) }()
			go func() { defer wg.Done(); slowPipe(out, in, delay) }()
		}
	}()
	return ln.Addr().String()
}

// slowPipe copies from to to, each read delay after it came, and closes
// both the moment from ends, dropping what it had yet to pass on.
func slowPipe(from, to net.Conn, delay time.Duration) {
	type chunk struct {
		at time.Time
		b  []byte
	}
	q := make(chan chunk, 4096)
	written := make(chan struct{})
	go func() {
		defer close(written)
		for c := range q {
			time.Sleep(time.Until(c.at.Add(delay)))
			if _, err := to.Write(c.b); err != nil {
				for range q {
				}
				return
			}
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			q <- chunk{time.Now(), bytes.Clone(buf[:n])}
		}
		if err != nil {
			from.Close()
			to.Close()
			close(q)
			<-written
			return
		}
	}
}
