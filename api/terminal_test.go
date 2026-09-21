//go:build linux || darwin

package api

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// These are about the terminal: a CLI on a pseudo-terminal that lives in the
// server, several people attached to it, and exactly one of them typing.
//
// The fake CLI is the interactive one — it says how big its window is,
// answers every line with a transformed copy so the terminal's own echo can
// be told from what the program printed, and exits on "bye" — so nothing
// here depends on a real shell's behaviour.

/* ------------------------------------------------------------ the harness --- */

// terms is a server with the terminal group on and three principals: one
// watcher and two people who may control, because most of what the keyboard
// does needs two of them.
type terms struct {
	*harness
	tt                *testing.T
	watch, alice, bob string
}

func withTerminals(t *testing.T, tune ...func(*Options)) *terms {
	t.Helper()
	watch, alice, bob := "watch-token-tttt", "alice-token-tttt", "bob-token-tttt"
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	opts := Options{
		Tokens: []TokenPrincipal{
			{Name: "looker", Role: RoleWatch, SHA256: sum(watch)},
			{Name: "alice", Role: RoleControl, SHA256: sum(alice)},
			{Name: "bob", Role: RoleControl, SHA256: sum(bob)},
		},
		Routes: &Routes{API: true, Playground: true, WebSocket: true, Health: true, Terminal: true},
	}
	for _, f := range tune {
		f(&opts)
	}
	h := newHarness(t, opts)
	ttyClaude(t)
	return &terms{harness: h, tt: t, watch: watch, alice: alice, bob: bob}
}

// ttyClaude puts the interactive fake on PATH as claude, ahead of the
// harness's own.
func ttyClaude(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Tty: true})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// start asks for one terminal and returns its description.
func (p *terms) start(body map[string]any) map[string]any {
	p.t.Helper()
	if body == nil {
		body = map[string]any{"account": 1, "cols": 120, "rows": 40}
	}
	resp, raw := p.do("POST", "/v1/terminals", body)
	if resp.StatusCode != 201 {
		p.t.Fatalf("starting a terminal: %d %s", resp.StatusCode, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		p.t.Fatal(err)
	}
	return doc
}

// as sends one request carrying a bearer token of the caller's choosing.
func (p *terms) as(token, method, path string, body any) (*http.Response, []byte) {
	p.tt.Helper()
	old := p.token
	p.token = token
	defer func() { p.token = old }()
	return p.do(method, path, body)
}

// waitViewers waits until a terminal has exactly this many connections, so
// a test about what happens after somebody leaves is not a test about how
// fast the server noticed.
func (p *terms) waitViewers(id string, n int) {
	p.tt.Helper()
	for range 500 {
		if list, _ := p.describe(id)["viewers"].([]any); len(list) == n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.tt.Fatalf("terminal %s never got down to %d viewers", id, n)
}

func (p *terms) describe(id string) map[string]any {
	p.t.Helper()
	resp, raw := p.do("GET", "/v1/terminals/"+id, nil)
	if resp.StatusCode != 200 {
		p.t.Fatalf("describing %s: %d %s", id, resp.StatusCode, raw)
	}
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	return doc
}

/* ---------------------------------------------------- a client of a terminal --- */

// term is one connection to a terminal: the socket, everything it has been
// shown, and every document it has been sent.
type term struct {
	t   *testing.T
	c   *wsClient
	out strings.Builder
	// seen is how far through the documents this connection's waits have
	// got. A terminal answers several things with an error frame, so a wait
	// that looked at everything again would keep finding the last refusal
	// and call it the answer to the next question.
	seen int
}

// attach opens a socket onto a terminal as whoever holds that token.
func (p *terms) attach(id, token, query string) *term {
	p.t.Helper()
	path := "/v1/terminals/" + id + "/ws"
	if query != "" {
		path += "?" + query
	}
	c, resp := dialWS(p.tt, p.harness, path, "Sec-WebSocket-Protocol", "rota, bearer."+token)
	if c == nil {
		p.tt.Fatalf("the handshake was refused: %d", resp.StatusCode)
	}
	return &term{t: p.tt, c: c}
}

// attachNarrow attaches over a connection whose receive window is a couple
// of kilobytes, set before the connection exists so the kernel cannot grow
// it afterwards. A reader that then stops reading really does stop the
// server's writes, rather than having a megabyte of buffer quietly absorb
// everything the test was counting on it not absorbing.
func (p *terms) attachNarrow(id, token string) *term {
	p.tt.Helper()
	wide := wsDial
	wsDial = func(addr string) (net.Conn, error) {
		d := &net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
			var err error
			if cerr := c.Control(func(fd uintptr) {
				err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
			}); cerr != nil {
				return cerr
			}
			return err
		}}
		return d.Dial("tcp", addr)
	}
	defer func() { wsDial = wide }()
	return p.attach(id, token, "")
}

// step reads one frame, keeping the bytes of output apart from the
// documents: a terminal says both on the same socket and they are told apart
// by the framing.
func (tm *term) step() (byte, []byte) {
	op, payload := tm.c.read()
	if op == opBinary {
		tm.out.Write(payload)
	}
	return op, payload
}

// waitDoc finds a document that satisfies want, among those already read and
// then among those still to come.
func (tm *term) waitDoc(what string, want func(map[string]any) bool) map[string]any {
	tm.t.Helper()
	for tm.seen < len(tm.c.log) {
		doc := tm.c.log[tm.seen]
		tm.seen++
		if want(doc) {
			return doc
		}
	}
	for range 4000 {
		op, payload := tm.step()
		switch op {
		case opText:
			tm.seen = len(tm.c.log)
			if doc := tm.c.log[len(tm.c.log)-1]; want(doc) {
				return doc
			}
		case opClose:
			tm.t.Fatalf("the socket closed with %d before %s arrived:\n%s",
				closeCode(payload), what, tm.c.all())
		}
	}
	tm.t.Fatalf("%s never arrived:\n%s", what, tm.c.all())
	return nil
}

func (tm *term) waitType(kind string) map[string]any {
	tm.t.Helper()
	return tm.waitDoc("a "+kind+" frame", func(doc map[string]any) bool { return doc["type"] == kind })
}

// waitOut reads until the terminal has printed something.
func (tm *term) waitOut(want string) {
	tm.t.Helper()
	for range 8000 {
		if strings.Contains(tm.out.String(), want) {
			return
		}
		if op, payload := tm.step(); op == opClose {
			tm.t.Fatalf("the socket closed with %d before %q was printed; it had said:\n%s",
				closeCode(payload), want, tm.printed())
		}
	}
	tm.t.Fatalf("%q was never printed; the terminal said:\n%s", want, tm.printed())
}

func (tm *term) printed() string { return tm.out.String() }

// typeIn sends the bytes of somebody typing, as a binary frame.
func (tm *term) typeIn(s string) {
	tm.t.Helper()
	tm.c.send(0x80|opBinary, []byte(s))
}

func (tm *term) say(v any) { tm.c.say(v) }

// readFrame is the client's frame reader without the fatal, for the two
// places a test reads outside its own goroutine or past the end of a
// connection: neither may call t.Fatalf, and a socket the server has closed
// is an answer rather than a failure.
func readFrame(c *wsClient) (byte, []byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(head[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return 0, nil, err
	}
	return head[0] & 0x0f, payload, nil
}

// endedWith reads this connection through whatever the server had already
// written and says how it closed it, or zero if it never did.
//
// Reading to the end matters: a connection the server gave up on may have a
// socketful of output in front of the close frame, and the server's own
// write of that frame is waiting behind it.
func (tm *term) endedWith() uint16 {
	tm.t.Helper()
	for range 20000 {
		op, payload, err := readFrame(tm.c)
		if err != nil {
			return 0
		}
		if op == opClose {
			return closeCode(payload)
		}
	}
	return 0
}

// busy is a connection somebody is reading as fast as it arrives, on a
// goroutine of its own.
//
// A test about one reader falling behind needs the other one never to: with
// the test reading only when it gets round to it, the server ends up waiting
// on the test, and which connection fills up first becomes a question about
// the machine. Nothing here calls t.Fatalf — a goroutine that is not the
// test's may not — so the end of a connection is recorded and reported by
// whoever was waiting.
type busy struct {
	mu   sync.Mutex
	out  strings.Builder
	code uint16
	over bool
}

// drain starts reading a connection and keeps everything it is shown.
func drain(c *wsClient) *busy {
	b := &busy{}
	go func() {
		for {
			op, payload, err := readFrame(c)
			b.mu.Lock()
			switch {
			case err != nil:
				b.over = true
			case op == opBinary:
				b.out.Write(payload)
			case op == opClose:
				b.code, b.over = closeCode(payload), true
			}
			done := b.over
			b.mu.Unlock()
			if done {
				return
			}
		}
	}()
	return b
}

func (b *busy) printed() (seen string, over bool, code uint16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.String(), b.over, b.code
}

// waitOut waits for this connection to have been shown something.
func (b *busy) waitOut(t *testing.T, want string) {
	t.Helper()
	for range 2000 {
		switch seen, over, code := b.printed(); {
		case strings.Contains(seen, want):
			return
		case over:
			t.Fatalf("the socket ended with %d before %q was printed; it had said:\n%s", code, want, seen)
		}
		time.Sleep(5 * time.Millisecond)
	}
	seen, _, _ := b.printed()
	t.Fatalf("%q was never printed; the terminal said:\n%s", want, seen)
}

/* ------------------------------------------------------ starting and seeing --- */

// A terminal is started, somebody attaches, and what the CLI printed on its
// pseudo-terminal arrives as bytes — with the size it was given, which the
// fake reads off the device rather than out of its environment.
func TestATerminalPrintsWhatItsCLIPrintsAtTheSizeItWasGiven(t *testing.T) {
	p := withTerminals(t)
	doc := p.start(map[string]any{"account": 1, "cols": 120, "rows": 40, "label": "fintech"})
	id, _ := doc["id"].(string)
	if id == "" {
		t.Fatalf("a terminal with no id: %v", doc)
	}
	tm := p.attach(id, p.alice, "")
	hello := tm.waitType("hello")
	for key, want := range map[string]any{
		"id": id, "kind": "account", "cols": float64(120), "rows": float64(40),
		"offset": float64(0), "holder": "alice",
	} {
		if hello[key] != want {
			t.Errorf("hello %s is %v, want %v", key, hello[key], want)
		}
	}
	you, _ := hello["you"].(map[string]any)
	if you["name"] != "alice" || you["role"] != "control" || you["conn"] == "" {
		t.Errorf("hello says you are %v", you)
	}
	tm.waitOut("ready 120x40")

	// And the description is what the page's panel is built from.
	got := p.describe(id)
	if got["label"] != "fintech" || got["holder"] != "alice" || got["ended"] != false {
		t.Errorf("the description is %v", got)
	}
	acc, _ := got["account"].(map[string]any)
	if acc["id"] != float64(1) || acc["provider"] != "claude" || acc["label"] != "a@x" {
		t.Errorf("the account is %v", acc)
	}
	if got["recording"] != false {
		t.Errorf("recording is %v, and nothing asked for one", got["recording"])
	}
	viewers, _ := got["viewers"].([]any)
	if len(viewers) != 1 {
		t.Errorf("viewers are %v", viewers)
	}
}

// The holder types and the CLI answers. The answer is the transformed copy,
// which is how it is told apart from the terminal's own echo.
func TestTheHolderTypesAndTheCLIAnswers(t *testing.T) {
	p := withTerminals(t)
	tm := p.attach(p.start(nil)["id"].(string), p.alice, "")
	tm.waitOut("ready ")
	tm.typeIn("hello there\n")
	tm.waitOut("got:hello there")
}

/* --------------------------------------------------------- who may type --- */

// A watcher is a watcher in fact: its bytes are refused, they never reach
// the CLI, and the terminal carries on for everybody else.
func TestAWatchersBytesNeverReachTheCLI(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	holder := p.attach(id, p.alice, "")
	holder.waitOut("ready ")

	looker := p.attach(id, p.watch, "")
	looker.waitType("hello")
	looker.typeIn("watcher-was-here\n")
	if msg := looker.waitType("error")["message"]; msg != watchOnly {
		t.Fatalf("a watcher typing is refused with %q, want %q", msg, watchOnly)
	}
	// A resize is the other thing only the holder may do.
	looker.say(map[string]any{"type": "resize", "cols": 10, "rows": 10})
	if msg := looker.waitType("error")["message"]; msg != watchOnly {
		t.Fatalf("a watcher resizing is refused with %q", msg)
	}

	// The proof is what the CLI did with it: nothing. Something that went in
	// after it has come back, so anything the watcher sent would be here.
	holder.typeIn("after the watcher\n")
	holder.waitOut("got:after the watcher")
	if strings.Contains(holder.printed(), "watcher-was-here") {
		t.Fatalf("the watcher's bytes reached the CLI:\n%s", holder.printed())
	}
	if strings.Contains(holder.printed(), "size 10x10") {
		t.Fatalf("the watcher resized everybody's terminal:\n%s", holder.printed())
	}
}

// A control principal without the keyboard is refused too, and told which of
// the two it is: it may have the keyboard, it simply does not hold it.
func TestControlWithoutTheKeyboardIsRefusedAndSaysWhy(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitOut("ready ")

	bob := p.attach(id, p.bob, "")
	if holder := bob.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("the keyboard is %v, and alice was there first", holder)
	}
	bob.typeIn("bob-was-here\n")
	if msg := bob.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("bob is refused with %q, want %q", msg, notHolding)
	}

	// And after losing it: alice releases, bob takes it, alice is refused.
	alice.say(map[string]any{"type": "release"})
	alice.waitDoc("the keyboard freed", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == nil
	})
	bob.say(map[string]any{"type": "claim"})
	bob.waitDoc("the keyboard taken", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "bob"
	})
	alice.typeIn("alice-again\n")
	if msg := alice.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("alice, having let go, is refused with %q", msg)
	}
	bob.typeIn("bob-now\n")
	bob.waitOut("got:bob-now")
	if s := bob.printed(); strings.Contains(s, "bob-was-here") || strings.Contains(s, "alice-again") {
		t.Fatalf("something refused still reached the CLI:\n%s", s)
	}
}

// A control principal may attach to watch, and then it is one: the keyboard
// is not offered to it and it cannot ask for it on that connection.
func TestControlMayAttachToWatchAndThenMayNotType(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	over := p.attach(id, p.alice, "mode=watch")
	if holder := over.waitType("hello")["holder"]; holder != nil {
		t.Fatalf("a connection that asked to watch took the keyboard: %v", holder)
	}
	over.typeIn("no\n")
	if msg := over.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("typing on it is refused with %q, want %q", msg, notHolding)
	}
	over.say(map[string]any{"type": "claim"})
	if msg := over.waitType("error")["message"]; msg != watchConn {
		t.Fatalf("asking for the keyboard on it is refused with %q, want %q", msg, watchConn)
	}
}

/* ---------------------------------------------------------- the keyboard --- */

// Asking, being refused, being given it, and taking it when nobody answers.
func TestClaimingTheKeyboard(t *testing.T) {
	p := withTerminals(t)
	var mu sync.Mutex
	at := time.Now()
	p.api.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })
	forward := func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }

	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitType("hello")
	bob := p.attach(id, p.bob, "")
	bob.waitType("hello")

	// Asking puts the request in front of the holder, by name and by the id
	// of the connection that asked.
	bob.say(map[string]any{"type": "claim"})
	ask := alice.waitType("claim_request")
	if ask["by"] != "bob" || ask["id"] == "" {
		t.Fatalf("the request is %v", ask)
	}
	// Refused: the asker is told, and forcing now means asking again first.
	alice.say(map[string]any{"type": "deny", "to": ask["id"]})
	if msg := bob.waitType("error")["message"]; msg != "alice kept the keyboard" {
		t.Fatalf("a denial reads %q", msg)
	}
	bob.say(map[string]any{"type": "claim", "force": true})
	if msg, _ := bob.waitType("error")["message"].(string); !strings.Contains(msg, "ask for the keyboard first") {
		t.Fatalf("forcing without asking reads %q", msg)
	}

	// Asked again and answered: alice hands it over.
	bob.say(map[string]any{"type": "claim"})
	ask = alice.waitType("claim_request")
	alice.say(map[string]any{"type": "grant", "to": ask["id"]})
	alice.waitDoc("bob holding the keyboard", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "bob"
	})
	bob.typeIn("mine now\n")
	bob.waitOut("got:mine now")

	// Alice asks back. Forcing before the wait is refused with what is left
	// of it; after it, the keyboard is hers.
	alice.say(map[string]any{"type": "claim"})
	bob.waitType("claim_request")
	forward(3 * time.Second)
	alice.say(map[string]any{"type": "claim", "force": true})
	msg, _ := alice.waitType("error")["message"].(string)
	if !strings.Contains(msg, "force takes the keyboard in 7s") {
		t.Fatalf("forcing too early reads %q", msg)
	}
	forward(claimWait)
	alice.say(map[string]any{"type": "claim", "force": true})
	bob.waitDoc("alice forcing the keyboard", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "alice"
	})
	alice.typeIn("forced\n")
	alice.waitOut("got:forced")
}

// A dropped holder keeps the keyboard for the grace, so a reconnect gets it
// back rather than finding somebody else typing.
func TestTheKeyboardSurvivesTheHoldersReconnect(t *testing.T) {
	p := withTerminals(t)
	var mu sync.Mutex
	at := time.Now()
	p.api.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })
	forward := func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }

	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitType("hello")
	alice.c.conn.Close() // the lid closed, the proxy gave up
	p.waitViewers(id, 0)

	// Within the grace, it is still hers: bob attaching does not get it.
	bob := p.attach(id, p.bob, "")
	if holder := bob.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("during the grace the keyboard is %v, want alice", holder)
	}
	// And she gets it back by coming back.
	again := p.attach(id, p.alice, "")
	if holder := again.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("on reconnecting alice finds the keyboard is %v", holder)
	}
	again.typeIn("back\n")
	again.waitOut("got:back")

	// Once the grace has passed with nobody there, it is free for whoever
	// asks next.
	again.c.conn.Close()
	bob.c.conn.Close()
	p.waitViewers(id, 0)
	forward(keyboardGrace + time.Second)
	third := p.attach(id, p.bob, "")
	if holder := third.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("after the grace the keyboard is %v, want bob", holder)
	}
}

// A resize is the holder's, it reaches the CLI, and everybody is told.
func TestTheHolderResizesAndEverybodyIsTold(t *testing.T) {
	p := withTerminals(t)
	id := p.start(map[string]any{"account": 1, "cols": 80, "rows": 24})["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitOut("ready 80x24")
	looker := p.attach(id, p.watch, "")
	looker.waitType("hello")

	alice.say(map[string]any{"type": "resize", "cols": 100, "rows": 30})
	for _, tm := range []*term{alice, looker} {
		got := tm.waitType("resized")
		if got["cols"] != float64(100) || got["rows"] != float64(30) {
			t.Fatalf("resized says %v", got)
		}
	}
	// The CLI was told by the kernel, and says so itself.
	alice.waitOut("size 100x30")
	if got := p.describe(id); got["cols"] != float64(100) || got["rows"] != float64(30) {
		t.Fatalf("the description still says %v x %v", got["cols"], got["rows"])
	}
}

// Attaching and leaving are told to everybody who is there, by name and role
// and never by address.
func TestViewersAreToldOfEachOther(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitType("hello")
	looker := p.attach(id, p.watch, "")
	looker.waitType("hello")

	list := alice.waitDoc("a viewers frame naming the watcher", func(d map[string]any) bool {
		if d["type"] != "viewers" {
			return false
		}
		raw, _ := json.Marshal(d["list"])
		return strings.Contains(string(raw), `"name":"looker"`)
	})
	raw, _ := json.Marshal(list["list"])
	for _, leak := range []string{"127.0.0.1", "[::1]"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("a viewer's address is nobody's business: %s", raw)
		}
	}
	looker.c.conn.Close()
	alice.waitDoc("a viewers frame without the watcher", func(d map[string]any) bool {
		if d["type"] != "viewers" {
			return false
		}
		raw, _ := json.Marshal(d["list"])
		return !strings.Contains(string(raw), `"name":"looker"`)
	})
}

/* ------------------------------------------------------- replay and gaps --- */

// A client that says where it got to is given what it missed, and told when
// what it missed is no longer there.
func TestReplayFromAnOffsetAndTheGapWhenItIsTooOld(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.Scrollback = 256 })
	id := p.start(nil)["id"].(string)
	first := p.attach(id, p.alice, "")
	first.waitOut("ready ")
	first.typeIn("one\n")
	first.waitOut("got:one")
	at := int64(p.describe(id)["offset"].(float64))

	// Everything after that offset, and nothing before it.
	first.typeIn("two\n")
	first.waitOut("got:two")
	second := p.attach(id, p.alice, fmt.Sprintf("since=%d&mode=watch", at))
	if off := int64(second.waitType("hello")["offset"].(float64)); off != at {
		t.Fatalf("the replay starts at %d, want %d", off, at)
	}
	second.waitOut("got:two")
	if strings.Contains(second.printed(), "got:one") {
		t.Fatalf("it was given what it already had:\n%s", second.printed())
	}

	// Asked for something the ring has thrown away, it is told so first and
	// given everything that is left.
	for i := range 12 {
		first.typeIn(fmt.Sprintf("line %02d ------------------------------\n", i))
	}
	first.waitOut("got:line 11")
	third := p.attach(id, p.alice, "since=0&mode=watch")
	hello := third.waitType("hello")
	gap := third.waitType("gap")
	if gap["from"] != float64(0) || gap["to"].(float64) <= 0 {
		t.Fatalf("the gap is %v", gap)
	}
	if hello["offset"] != gap["to"] {
		t.Fatalf("hello starts at %v and the gap ends at %v", hello["offset"], gap["to"])
	}
}

// One reader that has stopped reading is closed, and everybody else carries
// on: a terminal never waits for a socket.
func TestASlowWatcherIsDroppedAndTheHolderKeepsWorking(t *testing.T) {
	// Only the connection that is going to stop reading is given a shallow
	// queue; the holder is given one deeper than this terminal could fill,
	// so which of the two is dropped is settled by which of them is reading
	// and by nothing else. Shortening every queue alike would make this a
	// test about how promptly the machine got round to the holder.
	was := termQueueFor
	termQueueFor = func(_ *Principal, watch bool) int {
		if watch {
			return 4
		}
		return 1 << 14
	}
	t.Cleanup(func() { termQueueFor = was })

	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	// The holder's socket is read from before the noise starts and goes on
	// being read: the server must never have to wait for the holder, or the
	// holder is what this test would be about.
	held := drain(alice.c)
	held.waitOut(t, "ready ")

	looker := p.attachNarrow(id, p.watch)
	// Attached, and not reading another byte from here on.
	p.waitViewers(id, 2)

	// Four megabytes: past anything a kernel will buffer for a socket
	// nobody is reading, so the watcher's queue fills for certain rather
	// than for luck.
	alice.typeIn("noise 20000\n")
	held.waitOut(t, "noise done")
	if code := looker.endedWith(); code != wsTooSlow {
		t.Fatalf("a watcher that stopped reading was closed with %d, want %d", code, wsTooSlow)
	}
	// And the holder never noticed.
	alice.typeIn("still here\n")
	held.waitOut(t, "got:still here")
}

/* ------------------------------------------------------------ the ending --- */

// The CLI exiting is told to everyone, and the terminal stays listed as
// ended so a client that reattaches late reads the exit rather than a 404.
func TestTheExitIsBroadcastAndTheTerminalStaysListed(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	looker := p.attach(id, p.watch, "")
	alice.waitOut("ready ")
	looker.waitType("hello")

	alice.typeIn("bye\n")
	for _, tm := range []*term{alice, looker} {
		if code := tm.waitType("exit")["code"]; code != float64(0) {
			t.Fatalf("the exit says %v", code)
		}
	}
	for range 200 {
		if got := p.describe(id); got["ended"] == true {
			if got["exit"] != float64(0) {
				t.Fatalf("the ended terminal says %v", got)
			}
			// And somebody arriving late is given the end rather than a 404.
			late := p.attach(id, p.watch, "")
			late.waitType("exit")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the terminal never reported itself ended")
}

// Deleting one ends it, and says who did.
func TestDeletingATerminalEndsIt(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitOut("ready ")

	if resp, raw := p.as(p.alice, "DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != 200 {
		t.Fatalf("deleting: %d %s", resp.StatusCode, raw)
	}
	alice.waitType("exit")
	for range 200 {
		if p.describe(id)["ended"] == true {
			// A second delete is a terminal that has already ended.
			if resp, _ := p.as(p.alice, "DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != 409 {
				t.Fatalf("deleting it twice: %d", resp.StatusCode)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a deleted terminal never ended")
}

// Stopping the server stops the terminals: each one is a CLI in a session of
// its own, which is exactly what a signal to rota does not reach.
func TestStoppingTheServerEndsEveryTerminal(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitOut("ready ")

	p.api.Stop()
	for range 300 {
		if p.api.findTerminal(id).over() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a terminal outlived the server")
}

// A terminal nobody is attached to ends when the idle timeout passes. The
// clock is moved rather than waited for.
func TestAnUnattendedTerminalEndsWhenItIsIdleTooLong(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.IdleTimeout = time.Hour })
	var mu sync.Mutex
	at := time.Now()
	p.api.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })

	id := p.start(nil)["id"].(string)
	p.api.sweepTerminals()
	if p.api.findTerminal(id).over() {
		t.Fatal("a terminal started a moment ago is not idle")
	}
	mu.Lock()
	at = at.Add(2 * time.Hour)
	mu.Unlock()
	p.api.sweepTerminals()
	for range 300 {
		if p.api.findTerminal(id).over() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("an idle terminal was kept for ever")
}

/* --------------------------------------------------------------- limits --- */

func TestMaxSessionsIsACap(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.MaxSessions = 1 })
	p.start(nil)
	resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1})
	if resp.StatusCode != 409 || !strings.Contains(string(raw), "terminal.max_sessions") {
		t.Fatalf("a second terminal: %d %s", resp.StatusCode, raw)
	}
}

func TestAShellNeedsAskingFor(t *testing.T) {
	p := withTerminals(t)
	resp, raw := p.do("POST", "/v1/terminals", map[string]any{"kind": "shell"})
	if resp.StatusCode != 403 || !strings.Contains(string(raw), shellOff) {
		t.Fatalf("a shell nobody allowed: %d %s", resp.StatusCode, raw)
	}
	// Naming both is a request that cannot mean anything.
	resp, raw = p.do("POST", "/v1/terminals", map[string]any{"kind": "shell", "account": 1})
	if resp.StatusCode != 400 {
		t.Fatalf("a shell on an account: %d %s", resp.StatusCode, raw)
	}
}

func TestAShellRunsWhenItIsAllowed(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.Shell = true })
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("HOME", t.TempDir())
	doc := p.start(map[string]any{"kind": "shell"})
	if doc["kind"] != "shell" {
		t.Fatalf("the shell terminal says %v", doc)
	}
	if _, ok := doc["account"]; ok {
		t.Fatalf("a shell runs no account: %v", doc)
	}
	if resp, raw := p.do("DELETE", "/v1/terminals/"+doc["id"].(string), nil); resp.StatusCode != 200 {
		t.Fatalf("ending it: %d %s", resp.StatusCode, raw)
	}
}

// The directory a terminal starts in is confined by the same rule, and
// refused in the same words, as a run's.
func TestATerminalsDirectoryIsConfined(t *testing.T) {
	p := withTerminals(t)
	resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1, "cwd": t.TempDir()})
	if resp.StatusCode != 400 || !strings.Contains(string(raw), "outside the allowed directories") {
		t.Fatalf("a directory outside the roots: %d %s", resp.StatusCode, raw)
	}
	// Inside one, it is where the terminal starts.
	inside := filepath.Join(p.root, "sub")
	doc := p.start(map[string]any{"account": 1, "cwd": inside})
	if got, _ := doc["cwd"].(string); !strings.HasSuffix(got, "sub") {
		t.Fatalf("the terminal starts in %q", got)
	}
}

/* ----------------------------------------------------- recording and audit --- */

// A recording is exactly what was on the screen, and nothing else: it is
// written from the one place output is fanned out, so what a client was sent
// and what was kept are the same bytes.
func TestARecordingIsWhatWasOnTheScreen(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.Record = true })
	doc := p.start(nil)
	id := doc["id"].(string)
	if doc["recording"] != true {
		t.Fatalf("the terminal does not say it is being recorded: %v", doc)
	}
	tm := p.attach(id, p.alice, "")
	tm.waitOut("ready ")
	tm.typeIn("swordfish\n")
	tm.waitOut("got:swordfish")
	tm.typeIn("bye\n")
	tm.waitType("exit")

	path := filepath.Join(p.dir, "terminals", id+".out")
	var raw []byte
	for range 300 {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), "got:swordfish") {
			raw = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if raw == nil {
		t.Fatalf("nothing was recorded at %s", path)
	}
	if got := string(raw); !strings.HasPrefix(tm.printed(), got) && !strings.HasPrefix(got, tm.printed()) {
		t.Fatalf("the recording is not what was shown:\nrecorded %q\nshown %q", got, tm.printed())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the recording is mode %04o, and it is a transcript of somebody's session", perm)
	}
	if dir, err := os.Stat(filepath.Join(p.dir, "terminals")); err != nil || dir.Mode().Perm() != 0o700 {
		t.Fatalf("the directory is %v", dir)
	}
	// And a description beside it, so a recording found later says whose.
	var meta []byte
	for range 300 {
		meta, _ = os.ReadFile(filepath.Join(p.dir, "terminals", id+".json"))
		var back map[string]any
		if json.Unmarshal(meta, &back) == nil && back["id"] == id && back["ended"] == true {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the description beside it is %s", meta)
}

func TestARecordingStopsAtItsCap(t *testing.T) {
	p := withTerminals(t, func(o *Options) { o.Terminal.Record = true; o.Terminal.RecordMax = 64 })
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitOut("ready ")
	for i := range 8 {
		tm.typeIn(fmt.Sprintf("plenty of output %d\n", i))
	}
	tm.waitOut("got:plenty of output 7")
	tm.typeIn("bye\n")
	tm.waitType("exit")

	path := filepath.Join(p.dir, "terminals", id+".out")
	for range 300 {
		if fi, err := os.Stat(path); err == nil && fi.Size() == 64 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	fi, err := os.Stat(path)
	t.Fatalf("the recording is %v (%v), and the cap was 64", fi, err)
}

// logBuf is a log a test can read, safely: the server writes to it from
// several goroutines.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Every terminal is written down, and nothing of what was typed or printed
// is: a log outlives the session by a long way, and is read by people who
// were never meant to look over somebody's shoulder.
func TestTheAuditSaysWhatHappenedAndNotWhatWasTyped(t *testing.T) {
	var out logBuf
	p := withTerminals(t, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.waitOut("ready ")
	alice.typeIn("swordfish-is-the-password\n")
	alice.waitOut("got:swordfish")
	alice.say(map[string]any{"type": "release"})
	alice.waitDoc("the keyboard freed", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == nil
	})
	alice.c.conn.Close()
	p.do("DELETE", "/v1/terminals/"+id, nil)
	for range 300 {
		if p.api.findTerminal(id).over() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	log := out.String()
	for _, want := range []string{
		"terminal created", "terminal attached", "keyboard taken",
		"keyboard released", "terminal detached", "terminal killed", "terminal ended",
		"terminal=" + id, "kind=account", "by=token", "name=alice", "role=control",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the log never says %q:\n%s", want, log)
		}
	}
	for _, leak := range []string{"swordfish", "got:", "ready "} {
		if strings.Contains(log, leak) {
			t.Fatalf("the log carries %q, which is what was on the screen:\n%s", leak, log)
		}
	}
}

/* -------------------------------------------------------- the configuration --- */

func TestTerminalConfigRefusals(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"no api", "[routes]\napi = false\nplayground = false\nwebsocket = false\nterminal = true\n",
			"routes.terminal needs routes.api"},
		{"no socket", "[routes]\nterminal = true\nwebsocket = false\nplayground = false\n",
			"routes.terminal needs routes.websocket"},
		{"open to the network", "[routes]\nterminal = true\n[server]\nlisten = \"0.0.0.0:8787\"\n",
			"needs tls.cert and tls.key"},
		{"no sessions", "[terminal]\nmax_sessions = 0\n", "terminal.max_sessions must be more than nothing"},
		{"no scrollback", "[terminal]\nscrollback_bytes = 0\n", "terminal.scrollback_bytes must be more than nothing"},
		{"no recording", "[terminal]\nrecord_max_bytes = 0\n", "terminal.record_max_bytes must be more than nothing"},
		{"no idle", "[terminal]\nidle_timeout = \"0s\"\n", "terminal.idle_timeout must be longer than nothing"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want something saying %q", err, c.want)
			}
		})
	}
}

// The loopback is the one address a terminal may be served on without a
// certificate, and a certificate is what makes any other one allowed.
func TestATerminalOnTheNetworkNeedsTLS(t *testing.T) {
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"loopback", "[routes]\nterminal = true\n[server]\nlisten = \"127.0.0.1:8787\"\n", true},
		{"localhost", "[routes]\nterminal = true\n[server]\nlisten = \"localhost:8787\"\n", true},
		{"everywhere", "[routes]\nterminal = true\n[server]\nlisten = \"0.0.0.0:8787\"\n", false},
		{"a bare port", "[routes]\nterminal = true\n[server]\nlisten = \"8787\"\n", false},
		{"everywhere with a certificate",
			"[routes]\nterminal = true\n[server]\nlisten = \"0.0.0.0:8787\"\n[tls]\ncert = \"/c\"\nkey = \"/k\"\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadConfig(write(t, c.body))
			if c.ok != (err == nil) {
				t.Fatalf("got %v, wanted ok=%v", err, c.ok)
			}
		})
	}
}

// A server asked for the terminal without the socket it is carried on is
// half a feature, and refuses to start rather than answering routes nothing
// can attach to.
func TestHalfATerminalDoesNotStart(t *testing.T) {
	_, err := New(Options{Token: "x", Dir: t.TempDir(), RefreshEvery: -1,
		Routes: &Routes{API: true, Terminal: true}})
	if err == nil || !strings.Contains(err.Error(), "the terminal needs the API and the WebSocket routes") {
		t.Fatalf("got %v", err)
	}
}
