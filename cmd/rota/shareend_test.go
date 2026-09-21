//go:build linux || darwin

package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/pty"
)

// The whole of it, once, with nothing faked but the CLI.
//
// A real rota runs `run 1 --share` at a real pseudo-terminal, against a real
// server holding a real socket, with a real WebSocket attached from the
// other side. Everything below is arranged so that the one thing being
// asked is the one thing that matters: two people are typing at the same
// terminal — one at it, one at a browser — and both reach the CLI, and both
// see what it says back.

/* --------------------------------------------------- a socket to attach by --- */

// peer is the smallest WebSocket client that can hold a terminal: the
// handshake, masked frames out, whole frames in. It is here rather than
// borrowed because the one in the api package's tests is the server's own
// and this is meant to be somebody else.
type peer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader

	mu  sync.Mutex
	out strings.Builder
	doc []map[string]any
}

func attachTo(t *testing.T, base, path, token string) *peer {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	req := "GET " + path + " HTTP/1.1\r\nHost: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Authorization: Bearer " + token + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attaching was refused: %d", resp.StatusCode)
	}
	p := &peer{t: t, conn: conn, r: br}
	t.Cleanup(func() { conn.Close() })
	go p.read()
	return p
}

// read keeps everything: the bytes in one place and the documents in
// another, which is the whole of how this protocol tells them apart.
func (p *peer) read() {
	for {
		var head [2]byte
		if _, err := io.ReadFull(p.r, head[:]); err != nil {
			return
		}
		n := uint64(head[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(p.r, b[:]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(p.r, b[:]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(p.r, body); err != nil {
			return
		}
		p.mu.Lock()
		switch head[0] & 0x0f {
		case 0x1:
			var doc map[string]any
			if json.Unmarshal(body, &doc) == nil {
				p.doc = append(p.doc, doc)
			}
		case 0x2:
			p.out.Write(body)
		case 0x8:
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

// send writes one frame, masked as every frame from a client is.
func (p *peer) send(op byte, payload []byte) {
	p.t.Helper()
	mask := []byte{0x21, 0x7d, 0x0c, 0x5e}
	out := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		out = append(out, 0x80|byte(n))
	default:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	}
	out = append(out, mask...)
	body := append([]byte(nil), payload...)
	for i := range body {
		body[i] ^= mask[i%4]
	}
	if _, err := p.conn.Write(append(out, body...)); err != nil {
		p.t.Fatalf("writing a frame: %v", err)
	}
}

func (p *peer) printed() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *peer) waitOut(want string) {
	p.t.Helper()
	for range 2000 {
		if strings.Contains(p.printed(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("%q never reached the page; it was shown:\n%s", want, p.printed())
}

func (p *peer) waitDoc(kind string) map[string]any {
	p.t.Helper()
	for range 2000 {
		p.mu.Lock()
		for _, d := range p.doc {
			if d["type"] == kind {
				p.mu.Unlock()
				return d
			}
		}
		p.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	p.t.Fatalf("no %s frame ever arrived", kind)
	return nil
}

/* ---------------------------------------------------------- the whole thing --- */

func TestARealSharedTerminalIsTypedAtFromBothEnds(t *testing.T) {
	shortHome(t)
	home := os.Getenv("ROTA_HOME")
	// The terminals directory holds the socket, and a path too long for one
	// is a fact about where the temporary directory landed rather than
	// anything to fail over.
	if ln, err := net.Listen("unix", filepath.Join(home, "probe.sock")); err != nil {
		t.Skipf("no terminal can be shared here: %v", err)
	} else {
		ln.Close()
	}

	// The interactive fake, which says how big its window is, answers every
	// line with a transformed copy — so the terminal's own echo can be told
	// from what the program printed — and exits on "bye".
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Tty: true})

	s, err := api.New(api.Options{
		Dir: home, Token: "secret", RefreshEvery: -1,
		Routes: &api.Routes{API: true, Playground: true, WebSocket: true, Health: true, Terminal: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// A rota of its own, at a terminal of its own.
	cmd := exec.Command(os.Args[0], "run", "1", "--share", "--label", "sitting here")
	cmd.Env = append(os.Environ(),
		"ROTA_AS_RUN=1",
		"ROTA_HOME="+home,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	master, err := pty.Start(cmd, 90, 25)
	if err != nil {
		t.Fatalf("no pseudo-terminal for the sharing rota: %v", err)
	}
	screen := watchScreen(master)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		master.Close()
	})

	// rota says what it did before the CLI takes the screen, and the CLI
	// says how big the window it was given is — which is this one's.
	screen.wait(t, "shared to the rota server on this machine")
	screen.wait(t, "ready 90x25")

	id := waitForShared(t, srv.URL)

	// Somebody at the page, holding the keyboard because nobody else is.
	page := attachTo(t, srv.URL, "/v1/terminals/"+id+"/ws", "secret")
	page.waitDoc("hello")
	page.waitOut("ready 90x25")

	// Typed at the terminal itself.
	if _, err := master.Write([]byte("locally\r")); err != nil {
		t.Fatal(err)
	}
	screen.wait(t, "got:locally")
	page.waitOut("got:locally")

	// And typed from the page, which reaches the same CLI and comes back on
	// the same screen.
	page.send(0x2, []byte("from the page\r"))
	screen.wait(t, "got:from the page")
	page.waitOut("got:from the page")

	// Ending it at the terminal ends it for everybody, with the code.
	if _, err := master.Write([]byte("bye\r")); err != nil {
		t.Fatal(err)
	}
	if code := page.waitDoc("exit")["code"]; code != float64(0) {
		t.Fatalf("the page is told how it ended: %v", code)
	}
	if !strings.Contains(page.printed(), "bye") {
		t.Fatalf("after everything it printed: %q", page.printed())
	}
	// And rota exits with the CLI's own code rather than outliving it.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the sharing rota: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the sharing rota never exited with its CLI")
	}
	cmd.Process = nil // already waited for; the cleanup has nothing to do
}

// waitForShared waits until the server is holding a shared terminal and
// says which it is, with the things only a shared one has.
func waitForShared(t *testing.T, base string) string {
	t.Helper()
	for range 2000 {
		req, _ := http.NewRequest("GET", base+"/v1/terminals", nil)
		req.Header.Set("Authorization", "Bearer secret")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var doc struct {
				Terminals []map[string]any `json:"terminals"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			for _, row := range doc.Terminals {
				if row["kind"] != "shared" {
					continue
				}
				if row["label"] != "sitting here" || row["mode"] != "control" {
					t.Fatalf("a shared terminal says what it is: %v", row)
				}
				if row["cols"] != float64(90) || row["rows"] != float64(25) {
					t.Fatalf("and how big the window it is on is: %v", row)
				}
				if pid, _ := row["pid"].(float64); pid == 0 {
					t.Fatalf("and which process answers for it: %v", row)
				}
				id, _ := row["id"].(string)
				return id
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no terminal was ever shared to this server")
	return ""
}

// watchScreen reads the local terminal in the background, because a
// pseudo-terminal nobody drains fills and stops the process on it.
type shownOn struct {
	mu   sync.Mutex
	seen strings.Builder
}

func watchScreen(f *os.File) *shownOn {
	s := &shownOn{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.seen.Write(buf[:n])
				s.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *shownOn) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seen.String()
}

func (s *shownOn) wait(t *testing.T, want string) {
	t.Helper()
	for range 2000 {
		if strings.Contains(s.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%q never appeared at the terminal; it showed:\n%s", want, s.text())
}
