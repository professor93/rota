//go:build linux || darwin

package client

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	return &termRig{srv: srv, c: c, stop: stop}
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

// A server that stops says so, and Next returns what it said. The fake
// takes no notice of the hangup, so the only goodbye is the server's own.
func TestAServerCloseIsSaid(t *testing.T) {
	r := newTermRig(t, fakecli.Spec{Tty: true, Linger: "10s"})
	id := r.open(t, 80, 24)
	tm, err := r.c.Attach(id, false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	p := pumpOf(tm)
	p.output(t, "ready 80x24")
	r.srv.Stop()
	err = p.end(t, 5*time.Second)
	var closed *Closed
	if !errors.As(err, &closed) || closed.Code != 1001 {
		t.Fatalf("Next after the server stopped: %v, want a close 1001", err)
	}
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

/* ---------------------------------------------------- a hand-written peer --- */

// peer is a client whose every dial reaches script, in place of a server,
// once the handshake is answered and hello sent.
func peer(t *testing.T, script func(conn net.Conn, br *bufio.Reader)) *Client {
	t.Helper()
	c := newClient("a test peer")
	c.dial = func(context.Context) (net.Conn, error) {
		server, client := net.Pipe()
		go func() {
			defer server.Close()
			br := bufio.NewReader(server)
			req, err := http.ReadRequest(br)
			if err != nil {
				t.Errorf("the peer read no request: %v", err)
				return
			}
			if req.Host != "rota" || req.Header.Get("Sec-WebSocket-Version") != "13" {
				t.Errorf("the handshake: host %q, version %q", req.Host, req.Header.Get("Sec-WebSocket-Version"))
			}
			key, _ := base64.StdEncoding.DecodeString(req.Header.Get("Sec-WebSocket-Key"))
			if len(key) != 16 {
				t.Errorf("the key is %d bytes, want 16", len(key))
			}
			sum := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			_, _ = io.WriteString(server, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
				"Sec-WebSocket-Accept: "+base64.StdEncoding.EncodeToString(sum[:])+"\r\n\r\n")
			if err := peerWrite(server, 0x1, []byte(`{"type":"hello","id":"p","kind":"account","cols":80,"rows":24,"offset":0,"holder":null,"you":{"name":"me","role":"control","conn":"c1"},"viewers":[]}`)); err != nil {
				t.Errorf("the peer's hello: %v", err)
				return
			}
			script(server, br)
		}()
		return client, nil
	}
	return c
}

// peerWrite writes one frame as a server does, unmasked.
func peerWrite(w io.Writer, op byte, payload []byte) error {
	head := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n < 1<<16:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	_, err := w.Write(append(head, payload...))
	return err
}

// peerRead reads one frame as a server does, and says whether it was masked.
func peerRead(r io.Reader) (op byte, payload []byte, masked bool, err error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, false, err
	}
	op, masked = head[0]&0x0f, head[1]&0x80 != 0
	size := int(head[1] & 0x7f)
	if size >= 126 {
		return 0, nil, false, errors.New("a long frame where a short one was expected")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, false, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return op, payload, masked, nil
}

// A frame longer than maxFrame is refused before it is read, and the
// connection with it.
func TestAnOversizedFrameIsRefused(t *testing.T) {
	wrote := make(chan error, 1)
	c := peer(t, func(conn net.Conn, _ *bufio.Reader) {
		wrote <- peerWrite(conn, 0x2, make([]byte, 2<<20))
	})
	tm, err := c.Attach("p", false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if _, err := tm.Next(); err == nil {
		t.Fatal("a 2 MiB frame was read")
	}
	select {
	case err := <-wrote:
		if err == nil {
			t.Fatal("the peer wrote all 2 MiB: the connection was not closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the peer is still writing: the connection was not closed")
	}
	if _, err := tm.Next(); err == nil {
		t.Fatal("Next read on after refusing a frame")
	}
}

// A ping is answered with a masked pong carrying its payload, and is not a
// frame Next returns.
func TestPingsAreAnswered(t *testing.T) {
	pong := make(chan []byte, 1)
	c := peer(t, func(conn net.Conn, br *bufio.Reader) {
		go func() {
			_ = peerWrite(conn, 0x9, []byte("p1"))
			_ = peerWrite(conn, 0x2, []byte("x"))
		}()
		op, payload, masked, err := peerRead(br)
		if err != nil || op != 0xA || !masked {
			t.Errorf("the answer to a ping: op %#x masked %v err %v, want a masked pong", op, masked, err)
		}
		pong <- payload
		// Held open until the test is done reading.
		_, _, _, _ = peerRead(br)
	})
	tm, err := c.Attach("p", false)
	if err != nil {
		t.Fatal(err)
	}
	defer tm.Close()
	if h := tm.Hello(); h.ID != "p" || h.Holder != "" || h.You.Conn != "c1" {
		t.Fatalf("hello %+v", h)
	}
	f, err := tm.Next()
	if err != nil || f.Doc != nil || string(f.Output) != "x" {
		t.Fatalf("Next after a ping: %+v %v, want the output x", f, err)
	}
	select {
	case got := <-pong:
		if string(got) != "p1" {
			t.Fatalf("the pong carries %q, want p1", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong")
	}
}
