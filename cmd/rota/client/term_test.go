//go:build linux || darwin

package client

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/fakecli"
)

/* ------------------------------------------------------------ the rig --- */

// termRig is a real server with the terminal group on, reached in process
// as me, over a store whose one account runs the tty fake as claude.
type termRig struct {
	home string
	srv  *api.Server
	c    *Client
	stop func()
}

func newTermRig(t *testing.T, spec fakecli.Spec) *termRig {
	t.Helper()
	// Short, rather than t.TempDir: the store may hold a unix socket.
	home, err := os.MkdirTemp("", "r")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	work, err := os.MkdirTemp("", "w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	store := `{"accounts":[{"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"t"}}]}`
	if err := os.WriteFile(filepath.Join(home, "accounts.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROTA_HOME", home)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", spec)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	srv, err := api.New(api.Options{Token: "x", Dir: home, Roots: []string{work}, RefreshEvery: -1,
		Routes: &api.Routes{API: true, WebSocket: true, Terminal: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	c, stop := InProcess(srv.LocalHandler(&me))
	t.Cleanup(stop)
	return &termRig{home: home, srv: srv, c: c, stop: stop}
}

// open starts a terminal at this size and says its id.
func (r *termRig) open(t *testing.T, cols, rows uint16) string {
	t.Helper()
	s, err := r.c.Create(Create{Account: 1, Cols: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID
}

// pump reads one Term from one goroutine, as Next wants, and keeps
// everything it was shown.
type pump struct {
	frames chan Frame
	ended  chan error
	out    strings.Builder
}

func pumpOf(tm *Term) *pump {
	p := &pump{frames: make(chan Frame, 256), ended: make(chan error, 1)}
	go func() {
		for {
			f, err := tm.Next()
			if err != nil {
				p.ended <- err
				return
			}
			p.frames <- f
		}
	}()
	return p
}

// await reads frames until one satisfies want, for at most within.
func (p *pump) await(t *testing.T, what string, within time.Duration, want func(Frame) bool) Frame {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case f := <-p.frames:
			if f.Output != nil {
				p.out.Write(f.Output)
			}
			if want(f) {
				return f
			}
		case err := <-p.ended:
			t.Fatalf("waiting for %s, Next ended: %v (output so far %q)", what, err, p.out.String())
		case <-deadline:
			t.Fatalf("no %s within %s (output so far %q)", what, within, p.out.String())
		}
	}
}

// output waits until everything shown so far holds s.
func (p *pump) output(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(p.out.String(), s) {
		return
	}
	p.await(t, "output "+s, 5*time.Second, func(Frame) bool { return strings.Contains(p.out.String(), s) })
}

func (p *pump) doc(t *testing.T, typ string) *Doc {
	t.Helper()
	return p.await(t, "a "+typ+" document", 5*time.Second, func(f Frame) bool { return f.Doc != nil && f.Doc.Type == typ }).Doc
}

// end waits for Next to return an error.
func (p *pump) end(t *testing.T, within time.Duration) error {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case <-p.frames:
		case err := <-p.ended:
			return err
		case <-deadline:
			t.Fatalf("Next went on for %s", within)
		}
	}
}

/* ---------------------------------------------------------- the server --- */

// Attaching is hello first, at the terminal's own size; then the CLI's
// output, typing that reaches it, a fit that is answered, and a close that
// ends the reader.
func TestAttachSpeaksTheTerminalSocket(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	id := r.open(t, 90, 20)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	h := tm.Hello()
	if h.ID != id || h.Cols != 90 || h.Rows != 20 || h.Kind != "account" || h.You.Name != "me" || h.You.Role != "control" || h.You.Conn == "" {
		t.Fatalf("hello %+v, want %s at 90x20 to me", h, id)
	}
	p := pumpOf(tm)
	p.output(t, "ready 90x20")
	if err := tm.Type([]byte("hi\r")); err != nil {
		t.Fatal(err)
	}
	p.output(t, "got:hi")
	if err := tm.Fit(70, 15); err != nil {
		t.Fatal(err)
	}
	if d := p.doc(t, "resized"); d.Cols != 70 || d.Rows != 15 {
		t.Fatalf("resized %+v, want 70x15", d)
	}
	if err := tm.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.end(t, time.Second); err == nil {
		t.Fatal("Next ended without an error")
	}
	if err := tm.Type([]byte("x")); err == nil {
		t.Fatal("typing after Close was taken")
	}
	if err := tm.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// A watcher's typing is refused by the server, and never reaches the CLI:
// what a controller types after it is answered, and it never is.
func TestWatchTypesNothing(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	id := r.open(t, 80, 24)
	w, err := r.c.Attach(id, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	p := pumpOf(w)
	p.output(t, "ready 80x24")
	if err := w.Type([]byte("x\r")); err != nil {
		t.Fatal(err)
	}
	if d := p.doc(t, "error"); d.Message == "" {
		t.Fatalf("the refusal says nothing: %+v", d)
	}
	ctl, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	if err := ctl.Type([]byte("y\r")); err != nil {
		t.Fatal(err)
	}
	p.output(t, "got:y")
	if strings.Contains(p.out.String(), "got:x") {
		t.Fatalf("a watcher's typing reached the CLI: %q", p.out.String())
	}
}

func TestAnUnknownSessionIsARefusal(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	_, err := r.c.Attach("0000000000000000", false)
	var no *Refusal
	if !errors.As(err, &no) || no.Status != http.StatusNotFound || no.Message == "" {
		t.Fatalf("attaching to no session: %v, want a 404 refusal", err)
	}
}

// oneByte hands its reader one byte at a time, the way a slow link might.
type oneByte struct{ net.Conn }

func (o oneByte) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return o.Conn.Read(p)
}

func TestFramesSurviveOneByteReads(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	whole := r.c.dial
	r.c.dial = func(ctx context.Context) (net.Conn, error) {
		conn, err := whole(ctx)
		if err != nil {
			return nil, err
		}
		return oneByte{conn}, nil
	}
	id := r.open(t, 100, 30)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if h := tm.Hello(); h.ID != id || h.Cols != 100 || h.Rows != 30 {
		t.Fatalf("hello %+v", h)
	}
	p := pumpOf(tm)
	p.output(t, "ready 100x30")
	if err := tm.Fit(81, 25); err != nil {
		t.Fatal(err)
	}
	if d := p.doc(t, "resized"); d.Cols != 81 || d.Rows != 25 {
		t.Fatalf("resized %+v", d)
	}
	p.output(t, "size 81x25")
}

// A server that stops closes the socket: 1000 after the terminal's exit,
// or 1001 for the server going. Stop ends its terminals before it says it
// is going, so either may come first.
func TestAServerCloseIsSaid(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	id := r.open(t, 80, 24)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	p := pumpOf(tm)
	p.output(t, "ready 80x24")
	r.srv.Stop()
	err = p.end(t, 2*time.Second)
	var closed *Closed
	if !errors.As(err, &closed) || (closed.Code != 1000 && closed.Code != 1001) {
		t.Fatalf("Next after the server stopped: %v, want a close 1000 or 1001", err)
	}
}

// The account's home a claude terminal runs in is the rig's own, and
// mirrors the tests' stand-in Claude Code directory: nothing in it is, or
// leads to, the person's real HOME.
func TestTheAccountHomeIsNotThePersons(t *testing.T) {
	standIn := os.Getenv("CLAUDE_CONFIG_DIR")
	if standIn == "" {
		t.Fatal("CLAUDE_CONFIG_DIR is not set: TestMain did not isolate the tests")
	}
	// Something to mirror, so the home holds a link to follow.
	if err := os.WriteFile(filepath.Join(standIn, "settings.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTermRig(t, fakecli.Spec{Tty: true})
	id := r.open(t, 200, 24)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	p := pumpOf(tm)
	p.output(t, "ready 200x24")
	if err := tm.Type([]byte("env CLAUDE_CONFIG_DIR\r")); err != nil {
		t.Fatal(err)
	}
	// The pty makes the fake's \r\n into \r\r\n.
	line := regexp.MustCompile(`env CLAUDE_CONFIG_DIR=([^\r\n]+)\r+\n`)
	var m []string
	p.await(t, "the CLI's CLAUDE_CONFIG_DIR", 5*time.Second, func(Frame) bool {
		m = line.FindStringSubmatch(p.out.String())
		return m != nil
	})
	home := resolved(t, m[1])
	if store := resolved(t, r.home); !within(home, store) {
		t.Fatalf("the account's home is %s, outside the rig's store %s", home, store)
	}
	real := resolved(t, personHome)
	tmp := resolved(t, os.TempDir())
	if personHome == "" || within(tmp, real) {
		t.Skipf("the temporary directory %s is under HOME %s, so nothing more can be told apart", tmp, real)
	}
	if within(home, real) {
		t.Fatalf("the account's home %s is under the person's HOME %s", home, real)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		to := resolved(t, filepath.Join(home, e.Name()))
		if within(to, real) {
			t.Errorf("%s in the account's home leads to %s, under the person's HOME", e.Name(), to)
		}
		if e.Name() == "settings.json" {
			linked = within(to, resolved(t, standIn))
		}
	}
	if !linked {
		t.Errorf("the account's home has no settings.json linked into the stand-in %s", standIn)
	}
}

// resolved is a path with every link in it followed.
func resolved(t *testing.T, path string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// within says whether path is dir or inside it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// stop ends what InProcess accepted, a socket taken over included: a reader
// blocked on it returns.
func TestStopClosesAnAttachedSocket(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true})
	id := r.open(t, 80, 24)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	p := pumpOf(tm)
	p.output(t, "ready 80x24")
	r.stop()
	if err := p.end(t, 2*time.Second); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Next after stop: %v, want io.ErrUnexpectedEOF", err)
	}
}
