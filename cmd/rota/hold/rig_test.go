//go:build linux || darwin

package hold

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
)

// What the session tests drive a session with: a fake CLI on two pipes, a
// session serving a socket of its own, and the daemon's end of a link.

/* ------------------------------------------------------------ the fake CLI --- */

// fakeChild is a CLI on two pipes instead of a pty: what it prints is what
// the session reads, and what the session writes is what it reads, if the
// test lets it read at all.
type fakeChild struct {
	outR, inR *io.PipeReader
	outW, inW *io.PipeWriter

	ignoreHUP bool

	mu     sync.Mutex
	sizes  []string
	sigs   []os.Signal
	sigAt  []time.Time
	typed  []byte
	typeAt []time.Time // when each byte of typed arrived
	code   int
	gone   chan struct{}
	once   sync.Once
}

func newFakeChild() *fakeChild {
	f := &fakeChild{gone: make(chan struct{})}
	f.outR, f.outW = io.Pipe()
	f.inR, f.inW = io.Pipe()
	return f
}

func (f *fakeChild) Read(p []byte) (int, error)  { return f.outR.Read(p) }
func (f *fakeChild) Write(p []byte) (int, error) { return f.inW.Write(p) }
func (f *fakeChild) Pid() int                    { return 4242 }
func (f *fakeChild) Wait() int                   { <-f.gone; return f.code }

func (f *fakeChild) Resize(cols, rows uint16) error {
	select {
	case <-f.gone:
		return errors.New("fake: the CLI has exited")
	default:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sizes = append(f.sizes, fmt.Sprintf("%dx%d", cols, rows))
	return nil
}

func (f *fakeChild) Signal(sig os.Signal) error {
	f.mu.Lock()
	f.sigs = append(f.sigs, sig)
	f.sigAt = append(f.sigAt, time.Now())
	f.mu.Unlock()
	switch {
	case sig == syscall.SIGKILL:
		f.exit(128 + 9)
	case sig == syscall.SIGHUP && !f.ignoreHUP:
		f.exit(128 + 1)
	}
	return nil
}

// Close is the pty master closing: neither pipe carries anything after it.
func (f *fakeChild) Close() error {
	f.outR.Close()
	f.inW.Close()
	return nil
}

// exit ends the CLI and, as its terminal closing would, its output.
func (f *fakeChild) exit(code int) {
	f.exitKeep(code)
	f.outW.Close()
}

// exitKeep ends the CLI but leaves its output open, as a terminal still held
// by something the CLI started would be.
func (f *fakeChild) exitKeep(code int) {
	f.once.Do(func() {
		f.code = code
		close(f.gone)
	})
}

// print writes what the CLI prints, and fails the test if the session does
// not take it: nothing may stop the session reading its CLI.
func (f *fakeChild) print(t *testing.T, s string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := f.outW.Write([]byte(s)); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("printing %q: %v", s, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the session stopped reading its CLI with %d bytes to go", len(s))
	}
}

// reads makes the CLI read its terminal, keeping what it read and when.
func (f *fakeChild) reads() {
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.inR.Read(buf)
			f.mu.Lock()
			now := time.Now()
			for range n {
				f.typeAt = append(f.typeAt, now)
			}
			f.typed = append(f.typed, buf[:n]...)
			f.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
}

// waitTyped waits until the CLI has read want, and says when its last byte
// arrived.
func (f *fakeChild) waitTyped(t *testing.T, want string) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		i := bytes.Index(f.typed, []byte(want))
		var at time.Time
		if i >= 0 {
			at = f.typeAt[i+len(want)-1]
		}
		got := string(f.typed)
		f.mu.Unlock()
		if i >= 0 {
			return at
		}
		if time.Now().After(deadline) {
			t.Fatalf("the CLI read %q, never %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *fakeChild) read() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.typed)
}

/* ---------------------------------------------------------------- the rig --- */

const testID = "0123456789abcdef"

type rig struct {
	s         *Session
	c         *fakeChild
	terminals string
	exits     chan int
	logMu     sync.Mutex
	logs      []string
	// panics is a panic the test makes on purpose, which the log may name.
	panics string
}

// newRig starts a session on a fake CLI, serving a socket in a directory
// short enough for one.
func newRig(t *testing.T, scrollback int, c *fakeChild) *rig {
	t.Helper()
	return newRigOf(t, scrollback, c, c)
}

// newRigOf is newRig with the session holding child, which is c or wraps it.
func newRigOf(t *testing.T, scrollback int, c *fakeChild, child Child) *rig {
	t.Helper()
	dir, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	r := &rig{c: c, terminals: filepath.Join(dir, "terminals"), exits: make(chan int, 1)}
	sd := holdwire.Dir(r.terminals, testID)
	if err := os.MkdirAll(sd, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: holdwire.SockPath(sd), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	r.s = Start(Config{
		ID: testID, Terminals: r.terminals, Cols: 80, Rows: 24, Scrollback: scrollback,
		Log: func(msg string, args ...any) {
			r.logMu.Lock()
			r.logs = append(r.logs, fmt.Sprintln(append([]any{msg}, args...)...))
			r.logMu.Unlock()
		},
		OnExit: func(code int) { r.exits <- code },
	}, child)
	go r.s.Serve(ln)
	t.Cleanup(func() {
		c.exit(0)
		select {
		case <-r.s.Done():
		case <-time.After(10 * time.Second):
			t.Errorf("the session never finished")
		}
		// Any panic logged fails the test, but for one it makes on
		// purpose and names in r.panics.
		for _, l := range r.logLines() {
			if strings.Contains(l, "panic") && (r.panics == "" || !strings.Contains(l, r.panics)) {
				t.Errorf("logged: %s", l)
			}
		}
	})
	return r
}

func (r *rig) logLines() []string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	return slices.Clone(r.logs)
}

// claim makes this test process the store's daemon, the one process a
// holder links to.
func claim(t *testing.T, terminals string) {
	t.Helper()
	release, err := holdwire.ClaimDaemon(terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// peer is the daemon's end of a link, driven by hand.
type peer struct {
	t *testing.T
	c *share.Conn
}

func (r *rig) dial(t *testing.T) *peer {
	t.Helper()
	conn, err := net.Dial("unix", holdwire.SockPath(holdwire.Dir(r.terminals, testID)))
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{t: t, c: share.NewConn(conn, 0)}
	t.Cleanup(func() { p.c.Close() })
	// A holder refuses a peer that is not the daemon before it reads a
	// thing, and may have closed by the time this is sent: what it answered
	// is read next, whatever became of the Hello.
	_ = p.c.SendJSON(holdwire.Hello, holdwire.HelloMsg{Majors: holdwire.Majors})
	return p
}

// link dials and expects to be taken: a Welcome, then a State.
func (r *rig) link(t *testing.T) *peer {
	t.Helper()
	p := r.dial(t)
	if kind, b := p.next(); kind != holdwire.Welcome {
		t.Fatalf("the holder answered %d %q, not a Welcome", kind, b)
	}
	if kind, b := p.next(); kind != holdwire.State {
		t.Fatalf("after the Welcome came %d %q, not a State", kind, b)
	}
	return p
}

func (p *peer) send(kind byte, b []byte) {
	p.t.Helper()
	if err := p.c.Send(kind, b); err != nil {
		p.t.Fatal(err)
	}
}

func (p *peer) json(kind byte, v any) {
	p.t.Helper()
	if err := p.c.SendJSON(kind, v); err != nil {
		p.t.Fatal(err)
	}
}

func (p *peer) since(off int64) { p.json(holdwire.Since, holdwire.SinceMsg{Offset: off}) }

// next is the next frame, Pings left out.
func (p *peer) next() (byte, []byte) {
	p.t.Helper()
	for {
		p.c.Deadline(time.Now().Add(5 * time.Second))
		kind, b, err := p.c.Read()
		if err != nil {
			p.t.Fatalf("reading the link: %v", err)
		}
		if kind != holdwire.Ping {
			return kind, b
		}
	}
}

// until skips frames until one of kind, and fails on any of fail.
func (p *peer) until(kind byte, fail ...byte) []byte {
	p.t.Helper()
	for {
		k, b := p.next()
		if k == kind {
			return b
		}
		if slices.Contains(fail, k) {
			p.t.Fatalf("frame %d %q came before %d", k, b, kind)
		}
	}
}

func (p *peer) decode(kind byte, v any, fail ...byte) {
	p.t.Helper()
	if err := share.Unmarshal(p.until(kind, fail...), v); err != nil {
		p.t.Fatal(err)
	}
}

// output reads Output frames until n bytes from offset from, with no gap.
func (p *peer) output(from int64, n int) string {
	p.t.Helper()
	var got []byte
	for len(got) < n {
		off, b, err := holdwire.OutputOf(p.until(holdwire.Output))
		if err != nil {
			p.t.Fatal(err)
		}
		if want := from + int64(len(got)); off != want {
			p.t.Fatalf("output at %d, want %d (so far %q)", off, want, got)
		}
		got = append(got, b...)
	}
	return string(got)
}

func (p *peer) screen(seq uint64) (holdwire.ScreenHead, string) {
	p.t.Helper()
	p.json(holdwire.ScreenAsk, holdwire.ScreenAskMsg{Seq: seq})
	h, repaint, err := holdwire.ScreenOf(p.until(holdwire.Screen))
	if err != nil {
		p.t.Fatal(err)
	}
	if h.Seq != seq {
		p.t.Fatalf("Screen %d answers ScreenAsk %d", h.Seq, seq)
	}
	return h, string(repaint)
}

// eof waits for the holder to close the link.
func (p *peer) eof() {
	p.t.Helper()
	for {
		p.c.Deadline(time.Now().Add(5 * time.Second))
		if _, _, err := p.c.Read(); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				p.t.Fatal("the link is still open")
			}
			return
		}
	}
}
