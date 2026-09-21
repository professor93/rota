//go:build linux || darwin

package api

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/share"
)

// These are about a terminal this server did not start: one somebody is
// sitting at, offered over the socket in the store by the rota running it.
//
// The sharer here is a fake, and it is a fake of the wire rather than of the
// code: it dials the real socket, sends the real frames and reads what comes
// back, so what is tested is the protocol as it is actually spoken. The
// local half — raw mode, the copy loops, the queue that drops — is tested
// where it lives, in the command.

/* ------------------------------------------------------------ the harness --- */

// shareSockOf is where this server listens, worked out the same way the
// server works it out: the store's terminals directory, which is also where
// recordings go.
func (p *terms) shareSockOf() string {
	return filepath.Join(p.dir, "terminals", share.SockName)
}

// waitForSocket waits until the server is listening, because New returns
// before the first connection can be accepted on a socket it has only just
// bound. Everything after it in a test depends on it being there.
func (p *terms) waitForSocket() string {
	p.tt.Helper()
	path := p.shareSockOf()
	for range 500 {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.tt.Fatalf("this server never listened at %s", path)
	return ""
}

// faker is a rota sitting at a terminal, as far as this server is concerned.
type faker struct {
	t    *testing.T
	c    *share.Conn
	id   string
	mu   sync.Mutex
	got  []frameSeen
	done chan struct{}
}

type frameSeen struct {
	kind byte
	data []byte
}

// offer dials the socket and says hello. want is what the server is expected
// to answer with; the refusal is returned rather than failed on, because
// several tests are about exactly which sentence comes back.
func (p *terms) offer(hello share.HelloMsg) (*faker, share.RefusedMsg) {
	p.tt.Helper()
	path := p.waitForSocket()
	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		p.tt.Fatalf("dialing the share socket: %v", err)
	}
	c := share.NewConn(conn, 0)
	c.Deadline(time.Now().Add(20 * time.Second))
	if hello.Version == 0 {
		hello.Version = share.Version
	}
	if err := c.SendJSON(share.Hello, hello); err != nil {
		p.tt.Fatalf("saying hello: %v", err)
	}
	kind, payload, err := c.Read()
	if err != nil {
		p.tt.Fatalf("reading the answer to hello: %v", err)
	}
	switch kind {
	case share.Refused:
		var no share.RefusedMsg
		if err := share.Unmarshal(payload, &no); err != nil {
			p.tt.Fatal(err)
		}
		c.Close()
		return nil, no
	case share.Welcome:
		var yes share.WelcomeMsg
		if err := share.Unmarshal(payload, &yes); err != nil {
			p.tt.Fatal(err)
		}
		c.Clear()
		f := &faker{t: p.tt, c: c, id: yes.Terminal, done: make(chan struct{})}
		go f.read()
		p.tt.Cleanup(func() { c.Close() })
		return f, share.RefusedMsg{}
	}
	p.tt.Fatalf("hello was answered with frame %d, which is neither a welcome nor a refusal", kind)
	return nil, share.RefusedMsg{}
}

// read keeps everything the server says, so a test can look for one frame
// among several without racing the order they arrive in.
func (f *faker) read() {
	defer close(f.done)
	for {
		kind, payload, err := f.c.Read()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.got = append(f.got, frameSeen{kind: kind, data: payload})
		f.mu.Unlock()
	}
}

// waitFor waits for one frame of this kind and hands back its payload.
func (f *faker) waitFor(kind byte, what string) []byte {
	f.t.Helper()
	for range 1000 {
		f.mu.Lock()
		for _, fr := range f.got {
			if fr.kind == kind {
				data := fr.data
				f.mu.Unlock()
				return data
			}
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("%s never arrived from the server", what)
	return nil
}

// typed is everything the server has sent as input, joined: a test cares
// about what the keyboard produced, not about how it was cut into frames.
func (f *faker) typed() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, fr := range f.got {
		if fr.kind == share.Input {
			b.Write(fr.data)
		}
	}
	return b.String()
}

func (f *faker) waitTyped(want string) {
	f.t.Helper()
	for range 1000 {
		if strings.Contains(f.typed(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("%q never reached the sharer; it was sent %q", want, f.typed())
}

func (f *faker) print(s string) {
	f.t.Helper()
	if err := f.c.Send(share.Output, []byte(s)); err != nil {
		f.t.Fatalf("printing: %v", err)
	}
}

/* -------------------------------------------------------- what it becomes --- */

// A terminal offered over the socket is a terminal like any other: it is
// listed, it can be attached to, what it prints is replayed and streamed,
// and what a holder types on the page reaches the machine it is on.
func TestASharedTerminalIsOneOfThisServersTerminals(t *testing.T) {
	p := withTerminals(t)
	f, no := p.offer(share.HelloMsg{
		Account: 1, AccountLabel: "a@x", Provider: "claude",
		Label: "api", Cwd: "/src/api", Cols: 100, Rows: 30, PID: 4242, Mode: share.ModeControl,
	})
	if no.Message != "" {
		t.Fatalf("this hello was refused: %s", no.Message)
	}
	if f.id == "" {
		t.Fatal("a welcome says which terminal this became")
	}

	// It is listed, and it says what it is.
	doc := p.describe(f.id)
	if doc["kind"] != termKindShared {
		t.Fatalf("a shared terminal is of kind shared: %v", doc["kind"])
	}
	if doc["mode"] != share.ModeControl || doc["pid"] != float64(4242) {
		t.Fatalf("the listing says who is answering for it and whether it may be typed into: %v", doc)
	}
	if doc["label"] != "api" || doc["cwd"] != "/src/api" {
		t.Fatalf("and what the person called it, and where it is: %v", doc)
	}
	if acct, _ := doc["account"].(map[string]any); acct == nil || acct["label"] != "a@x" {
		t.Fatalf("and whose account is paying for it: %v", doc)
	}
	if doc["cols"] != float64(100) || doc["rows"] != float64(30) {
		t.Fatalf("and how big the window it is on is: %v", doc)
	}
	if doc["lost"] != nil {
		t.Fatalf("a link that is up is not a lost one: %v", doc)
	}

	// What it prints reaches somebody attaching after it was printed, and
	// somebody attached while it is being printed.
	f.print("before\r\n")
	watcher := p.attach(f.id, p.watch, "")
	watcher.waitType("hello")
	watcher.waitOut("before")
	f.print("after\r\n")
	watcher.waitOut("after")

	// And what a holder types on the page reaches the machine it is on, as
	// input frames and as nothing else.
	holder := p.attach(f.id, p.alice, "")
	holder.waitType("hello")
	holder.typeIn("ls -l\r")
	f.waitTyped("ls -l\r")

	// A watcher's keystroke never becomes one.
	watcher.typeIn("rm -rf /")
	if msg := watcher.waitType("error")["message"]; msg != watchOnly {
		t.Fatalf("a watcher typing is refused with %q, want %q", msg, watchOnly)
	}
	if strings.Contains(f.typed(), "rm -rf") {
		t.Fatalf("a watcher's keystroke reached the sharer: %q", f.typed())
	}
}

// The size of a shared terminal is the size of the window somebody is
// sitting at. The page may not drag it, and says so; the sharer may, and
// everybody attached is told.
func TestASharedTerminalsSizeIsTheWindowsAndNotThePages(t *testing.T) {
	p := withTerminals(t)
	f, _ := p.offer(share.HelloMsg{Account: 1, Cols: 80, Rows: 24})
	holder := p.attach(f.id, p.alice, "")
	holder.waitType("hello")

	holder.say(map[string]any{"type": "resize", "cols": 200, "rows": 60})
	if msg := holder.waitType("error")["message"]; msg != sharedSize {
		t.Fatalf("a resize on a shared terminal is refused with %q, want %q", msg, sharedSize)
	}
	if doc := p.describe(f.id); doc["cols"] != float64(80) {
		t.Fatalf("and changes nothing: %v", doc)
	}

	// The sharer's own window, though, is broadcast exactly as the server's
	// own terminals broadcast theirs.
	if err := f.c.SendJSON(share.Resized, share.ResizedMsg{Cols: 132, Rows: 43}); err != nil {
		t.Fatal(err)
	}
	got := holder.waitType("resized")
	if got["cols"] != float64(132) || got["rows"] != float64(43) {
		t.Fatalf("the window was dragged and everybody was told: %v", got)
	}
	for range 200 {
		if doc := p.describe(f.id); doc["cols"] == float64(132) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the description never caught up with the window")
}

// A terminal shared to be watched has no keyboard at all. That is the
// sharer's decision, taken where the terminal is, and a control sign-in on
// this server does not overrule it.
func TestATerminalSharedToBeWatchedTakesNothingFromThePage(t *testing.T) {
	p := withTerminals(t)
	f, _ := p.offer(share.HelloMsg{Account: 1, Mode: share.ModeWatch})
	if doc := p.describe(f.id); doc["mode"] != share.ModeWatch {
		t.Fatalf("the listing says it is to be watched: %v", doc)
	}

	c := p.attach(f.id, p.alice, "")
	hello := c.waitType("hello")
	if hello["holder"] != nil {
		t.Fatalf("nobody takes the keyboard of a terminal that has none: %v", hello)
	}
	c.say(map[string]any{"type": "claim"})
	if msg := c.waitType("error")["message"]; msg != sharedWatch {
		t.Fatalf("claiming it is refused with %q, want %q", msg, sharedWatch)
	}
	c.typeIn("whoami\r")
	if msg := c.waitType("error")["message"]; msg != sharedWatch {
		t.Fatalf("typing into it is refused with %q, want %q", msg, sharedWatch)
	}
	// And nothing reached the machine it is on.
	f.print("hello\r\n")
	c.waitOut("hello")
	if f.typed() != "" {
		t.Fatalf("a watch-only terminal was sent input: %q", f.typed())
	}
}

// Ending one: DELETE is a kill frame, because this server has no process to
// signal — the sharer hangs up the CLI it is holding.
func TestKillingASharedTerminalAsksTheSharerToHangUp(t *testing.T) {
	p := withTerminals(t)
	f, _ := p.offer(share.HelloMsg{Account: 1})
	resp, raw := p.do("DELETE", "/v1/terminals/"+f.id, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("killing it: %d %s", resp.StatusCode, raw)
	}
	f.waitFor(share.Kill, "a kill frame")
}

// The two ways a shared terminal ends. An exit frame is the CLI having
// exited, with its code; the link breaking without one is the rota holding
// the terminal having gone, which has no code and says so in words.
func TestASharedTerminalEndsWithAnExitOrWithTheSharerGoingAway(t *testing.T) {
	p := withTerminals(t)
	f, _ := p.offer(share.HelloMsg{Account: 1})
	c := p.attach(f.id, p.alice, "")
	c.waitType("hello")
	f.print("bye\r\n")
	if err := f.c.SendJSON(share.Exit, share.ExitMsg{Code: 7}); err != nil {
		t.Fatal(err)
	}
	// The exit frame arrives, and it arrives after the output it follows:
	// a client that learned the terminal was over without learning what it
	// last printed would have lost the end of it.
	if code := c.waitType("exit")["code"]; code != float64(7) {
		t.Fatalf("the exit says how it ended: %v", code)
	}
	if !strings.Contains(c.printed(), "bye") {
		t.Fatalf("and everything it printed came first: %q", c.printed())
	}
	if doc := p.describe(f.id); doc["exit"] != float64(7) || doc["ended"] != true {
		t.Fatalf("the description says so too: %v", doc)
	}

	// And the other ending, on a second terminal.
	g, _ := p.offer(share.HelloMsg{Account: 1})
	gone := p.attach(g.id, p.alice, "")
	gone.waitType("hello")
	g.c.Close()
	if code := gone.waitType("exit")["code"]; code != float64(-1) {
		t.Fatalf("a link that broke has no code to report: %v", code)
	}
	doc := p.describe(g.id)
	if doc["lost"] != true || doc["ending"] != lostSharer {
		t.Fatalf("and the description says which of the two endings it was: %v", doc)
	}
}

// Output the sharer could not send is a gap here, in the same frame and the
// same words a reader gets for a scrollback that has moved on — and the
// offsets stay what they were, because those bytes were never counted.
func TestAGapAtTheSharerIsAGapForEverybodyWatching(t *testing.T) {
	p := withTerminals(t)
	f, _ := p.offer(share.HelloMsg{Account: 1})
	c := p.attach(f.id, p.alice, "")
	c.waitType("hello")
	f.print("one")
	c.waitOut("one")
	if err := f.c.SendJSON(share.Gap, share.GapMsg{Bytes: 4096}); err != nil {
		t.Fatal(err)
	}
	gap := c.waitType("gap")
	if gap["bytes"] != float64(4096) {
		t.Fatalf("the gap says how much was never sent: %v", gap)
	}
	if gap["from"] != gap["to"] {
		t.Fatalf("and does not move the offsets, which never counted those bytes: %v", gap)
	}
	f.print("two")
	c.waitOut("two")
	for range 200 {
		if doc := p.describe(f.id); doc["offset"] == float64(6) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the offset counts what arrived and nothing else: %v", p.describe(f.id))
}

/* ------------------------------------------------------- what is refused --- */

// A sharer speaking another version of the protocol is refused in a sentence
// it prints, and told not to come back: the two ends are the same program,
// so the answer is to upgrade one of them.
func TestASharerOfAnotherVersionIsRefusedForGood(t *testing.T) {
	p := withTerminals(t)
	_, no := p.offer(share.HelloMsg{Version: share.Version + 99, Account: 1})
	if !strings.Contains(no.Message, "sharing protocol") {
		t.Fatalf("the refusal says what did not match: %q", no.Message)
	}
	if !no.Final {
		t.Fatal("a version mismatch is not worth retrying")
	}
}

// A shared terminal counts against max_sessions like every other, and the
// sentence that says so is one the sharer can print — and come back after.
func TestASharedTerminalCountsAgainstMaxSessions(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.MaxSessions = 1 })
	if _, no := p.offer(share.HelloMsg{Account: 1}); no.Message != "" {
		t.Fatalf("the first was refused: %s", no.Message)
	}
	_, no := p.offer(share.HelloMsg{Account: 1})
	if !strings.Contains(no.Message, "max_sessions") {
		t.Fatalf("the second says why: %q", no.Message)
	}
	if no.Final {
		t.Fatal("a server that is full now may not be full in a minute")
	}
}

/* --------------------------------------------------------- the socket itself --- */

// Who may offer a terminal is decided by the filesystem: a socket only its
// owner can write to, inside a directory only its owner can enter.
func TestTheShareSocketIsTheOwnersAlone(t *testing.T) {
	p := withTerminals(t)
	path := p.waitForSocket()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the socket is %o, want 600", mode)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the directory it is in is %o, want 700", mode)
	}
}

// Two ways to have no socket, and both are the file saying so: the terminal
// group off, or sharing off inside it.
func TestNoSocketWhereTheFileDidNotAskForOne(t *testing.T) {
	off := false
	for _, c := range []struct {
		what string
		tune func(*Options)
	}{
		{"the terminal group is off", func(o *Options) {
			o.Routes = &Routes{API: true, Playground: true, WebSocket: true, Health: true}
		}},
		{"sharing is off", func(o *Options) { o.Terminal.Share = &off }},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := withTerminals(t, c.tune)
			path := p.shareSockOf()
			// Nothing is waited for: a socket that is going to appear
			// appears while New is still running, and this runs after it.
			if _, err := os.Stat(path); err == nil {
				t.Fatalf("there is a socket at %s and %s", path, c.what)
			}
		})
	}
}

// A socket file with nothing behind it is what a server that was killed
// leaves. It is replaced rather than refused: only one rota serves a store
// at a time, so a file at that path can only be this server's own leftover,
// and one that stayed would turn away every sharer for as long as it sat
// there.
func TestAStaleSocketIsReplaced(t *testing.T) {
	dir := t.TempDir()
	terms := filepath.Join(dir, "terminals")
	if err := os.MkdirAll(terms, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(terms, share.SockName)
	// A real socket, whose listener is then closed without removing the
	// file: exactly what a kill -9 leaves behind.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	if uln, ok := ln.(*net.UnixListener); ok {
		uln.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the leftover was not left over: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "accounts.json"),
		[]byte(`{"accounts":[{"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"t"}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Dir: dir, Token: "secret", RefreshEvery: -1,
		Routes: &Routes{API: true, Playground: true, WebSocket: true, Health: true, Terminal: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)

	conn, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		t.Fatalf("the leftover was not replaced: %v", err)
	}
	defer conn.Close()
	c := share.NewConn(conn, 0)
	c.Deadline(time.Now().Add(20 * time.Second))
	if err := c.SendJSON(share.Hello, share.HelloMsg{Version: share.Version}); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := c.Read()
	if err != nil || kind != share.Welcome {
		t.Fatalf("nothing answered on the new socket: frame %d, %v", kind, err)
	}
	var yes share.WelcomeMsg
	if share.Unmarshal(payload, &yes) != nil || yes.Terminal == "" {
		t.Fatalf("the welcome says which terminal this became: %s", payload)
	}
}

/* ------------------------------------------------------------------ odds --- */

// describeRaw is describe without the terminal having to exist, for the two
// tests that look at a listing rather than at one terminal.
func (p *terms) listing() []map[string]any {
	p.tt.Helper()
	resp, raw := p.do("GET", "/v1/terminals", nil)
	if resp.StatusCode != 200 {
		p.tt.Fatalf("listing: %d %s", resp.StatusCode, raw)
	}
	var doc struct {
		Terminals []map[string]any `json:"terminals"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		p.tt.Fatal(err)
	}
	return doc.Terminals
}

// A shared terminal is in the listing beside the server's own, so a page
// that draws one draws both.
func TestASharedTerminalIsInTheListingBesideTheServersOwn(t *testing.T) {
	p := withTerminals(t)
	own := p.start(nil)
	f, _ := p.offer(share.HelloMsg{Account: 1, Label: "sitting here"})
	kinds := map[string]string{}
	for _, row := range p.listing() {
		id, _ := row["id"].(string)
		kind, _ := row["kind"].(string)
		kinds[id] = kind
	}
	if kinds[fmt.Sprint(own["id"])] != termKindAccount || kinds[f.id] != termKindShared {
		t.Fatalf("both kinds are listed, each as what it is: %v", kinds)
	}
}
