//go:build linux || darwin

package api

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These are about the people at a held session: whose terminal answers what,
// who types, who sets the size, and what a viewer that cannot keep up is
// sent. Every rule here is a held session's alone; a terminal this server
// runs itself keeps the rules terminal_test.go pins.

/* ------------------------------------------------------------ the harness --- */

// withPersons gives the server two people who sign in on the page, alice and
// bob, both of whom may control.
func withPersons(o *Options) {
	o.Users = []User{
		{Name: "alice", Role: RoleControl, Password: cheapHash()},
		{Name: "bob", Role: RoleControl, Password: cheapHash()},
	}
}

// signIn is one sign-in on the page, as a device of that person: the cookie
// it is given. Two calls are two devices.
func (p *terms) signIn(name string) string {
	p.tt.Helper()
	pr, err := p.api.sessions.start(Principal{Name: name, Role: RoleControl, Via: "user"}, 0)
	if err != nil {
		p.tt.Fatal(err)
	}
	return pr.session
}

// attachAs attaches as the page does once its person has signed in: by the
// session cookie, from this server's own origin.
func (p *terms) attachAs(id, cookie, query string) *term {
	p.tt.Helper()
	path := "/v1/terminals/" + id + "/ws"
	if query != "" {
		path += "?" + query
	}
	c, resp := dialWS(p.tt, p.harness, path, "Cookie", sessionCookie+"="+cookie, "Origin", p.srv.URL)
	if c == nil {
		p.tt.Fatalf("the handshake was refused: %d", resp.StatusCode)
	}
	return &term{t: p.tt, c: c}
}

// connOf is the id the server gave a connection, from its hello.
func connOf(hello map[string]any) string {
	you, _ := hello["you"].(map[string]any)
	id, _ := you["conn"].(string)
	return id
}

// recorded is every line the fake holder wrote about what it was sent.
func recorded(path string) []string {
	raw, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

// waitRecorded waits for the fake holder to have written one line down.
func waitRecorded(t *testing.T, path, line string) {
	t.Helper()
	for range 500 {
		for _, l := range recorded(path) {
			if l == line {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the holder never recorded %q; it recorded:\n%s", line, strings.Join(recorded(path), "\n"))
}

// typedInto is every byte the fake holder was given to type, in order.
func typedInto(path string) string {
	var b strings.Builder
	for _, line := range recorded(path) {
		if rest, ok := strings.CutPrefix(line, "input "); ok {
			s, _ := strconv.Unquote(rest)
			b.WriteString(s)
		}
	}
	return b.String()
}

// settled waits until the server has acted on everything a connection sent
// before: its frames are read in order, so the pong comes after them.
func (tm *term) settled() {
	tm.t.Helper()
	tm.say(map[string]any{"type": "ping"})
	tm.waitType("pong")
}

// errors is every error frame a connection has been sent so far.
func (tm *term) errors() []string {
	var out []string
	for _, doc := range tm.c.log {
		if doc["type"] == "error" {
			msg, _ := doc["message"].(string)
			out = append(out, msg)
		}
	}
	return out
}

/* ---------------------------------------------------------- the answers --- */

// A viewer's terminal answers the queries it sees replayed, and the holder
// answered them already when the CLI asked. Those answers are taken out of
// whatever a connection sends before anything else looks at it, so a
// watcher's terminal answering is not a watcher typing, and the holder's
// typing reaches the CLI without them.
func TestAViewersTerminalNeverAnswersTwice(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "record")
	p := withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec})
	id := p.start(nil)["id"].(string)
	holder := p.attach(id, p.alice, "")
	holder.waitType("hello")

	for _, w := range []*term{p.attach(id, p.watch, ""), p.attach(id, p.bob, "mode=watch")} {
		w.waitType("hello")
		w.typeIn("\x1b[?62;22c")
		w.settled()
		if errs := w.errors(); len(errs) > 0 {
			t.Fatalf("a watcher's terminal answering was refused as typing: %q", errs)
		}
	}
	// Anything else a watcher sends is still refused as it always was.
	looker := p.attach(id, p.watch, "")
	looker.waitType("hello")
	looker.typeIn("z")
	if msg := looker.waitType("error")["message"]; msg != watchOnly {
		t.Fatalf("a watcher typing is refused with %q, want %q", msg, watchOnly)
	}

	holder.typeIn("x\x1b[?62;22cy\r")
	holder.waitOut("got:xy")
	if got := typedInto(rec); got != "xy\r" {
		t.Fatalf("the holder was given %q to type, want %q", got, "xy\r")
	}
}

/* ---------------------------------------------------------- the keyboard --- */

// One person on two devices needs no ceremony: whichever of their
// connections types holds the keyboard from then on, and the one that held
// it is told. Another person is refused as ever, and two connections on one
// bearer token are not a person at all, so they keep the claim flow.
func TestTheKeyboardFollowsThePersonNotTheConnection(t *testing.T) {
	var log logBuf
	p := withHeld(t, nil, withPersons, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	id := p.start(nil)["id"].(string)
	a := p.attachAs(id, p.signIn("alice"), "")
	helloA := a.waitType("hello")
	if helloA["holder"] != "alice" || helloA["holder_conn"] != connOf(helloA) {
		t.Fatalf("the first device to attach holds the keyboard: %v", helloA)
	}
	b := p.attachAs(id, p.signIn("alice"), "")
	helloB := b.waitType("hello")
	if helloB["holder_conn"] != connOf(helloA) {
		t.Fatalf("attaching a second device moved the keyboard: %v", helloB)
	}

	// A frame that held only its terminal's answers is nothing at all: the
	// same person's other device answering a query it saw replayed does not
	// take the keyboard.
	b.typeIn("\x1b[?62;22c")
	b.settled()
	a.settled()
	for _, d := range a.c.log {
		if d["type"] == "keyboard" && d["holder_conn"] == connOf(helloB) {
			t.Fatalf("a frame of nothing but replies moved the keyboard: %v", d)
		}
	}

	b.typeIn("from b\r")
	b.waitOut("got:from b")
	if errs := b.errors(); len(errs) > 0 {
		t.Fatalf("the same person on another device was refused: %q", errs)
	}
	a.waitDoc("the keyboard on device B", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "alice" && d["holder_conn"] == connOf(helloB)
	})
	if !strings.Contains(log.String(), "keyboard moved") {
		t.Errorf("the move was not audited:\n%s", log.String())
	}
	// And back again, by typing on the first.
	a.typeIn("from a\r")
	a.waitOut("got:from a")
	b.waitDoc("the keyboard back on device A", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder_conn"] == connOf(helloA)
	})

	// A device of hers attached to watch never types.
	look := p.attachAs(id, p.signIn("alice"), "mode=watch")
	look.waitType("hello")
	look.typeIn("peek\r")
	if msg := look.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("a device attached to watch typing is refused with %q, want %q", msg, notHolding)
	}

	bob := p.attachAs(id, p.signIn("bob"), "")
	bob.waitType("hello")
	bob.typeIn("not mine\r")
	if msg := bob.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("another person typing is refused with %q, want %q", msg, notHolding)
	}

	// A bearer token is never a person: the same token twice is two
	// connections, and the second asks.
	other := p.start(nil)["id"].(string)
	first := p.attach(other, p.alice, "")
	if holder := first.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("the token's first connection does not hold the keyboard: %v", holder)
	}
	second := p.attach(other, p.alice, "")
	second.waitType("hello")
	second.typeIn("x")
	if msg := second.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("a token's second connection typing is refused with %q, want %q", msg, notHolding)
	}
	second.say(map[string]any{"type": "claim"})
	first.waitType("claim_request")
}

// The keyboard is lost by being taken, and coming back does not take it
// back: a connection that lost it and attaches again finds whoever took it
// still holding it, and typing does not move it either.
func TestALostKeyboardIsNotReclaimedByReconnecting(t *testing.T) {
	p := withHeld(t, nil, withPersons)
	var mu sync.Mutex
	at := time.Now()
	p.api.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })
	forward := func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }

	id := p.start(nil)["id"].(string)
	cookie := p.signIn("alice")
	alice := p.attachAs(id, cookie, "")
	if holder := alice.waitType("hello")["holder"]; holder != "alice" {
		t.Fatalf("the keyboard is %v, want alice", holder)
	}
	bob := p.attachAs(id, p.signIn("bob"), "")
	bob.waitType("hello")
	bob.say(map[string]any{"type": "claim"})
	alice.waitType("claim_request")
	forward(claimWait)
	bob.say(map[string]any{"type": "claim", "force": true})
	alice.waitDoc("bob forcing the keyboard", func(d map[string]any) bool {
		return d["type"] == "keyboard" && d["holder"] == "bob"
	})

	alice.c.conn.Close()
	p.waitViewers(id, 1)
	again := p.attachAs(id, cookie, "")
	if holder := again.waitType("hello")["holder"]; holder != "bob" {
		t.Fatalf("alice coming back finds the keyboard is %v, want bob", holder)
	}
	again.typeIn("mine\r")
	if msg := again.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("alice typing after coming back is refused with %q, want %q", msg, notHolding)
	}
	bob.typeIn("still mine\r")
	bob.waitOut("got:still mine")
}

/* --------------------------------------------------------------- the size --- */

// A held session's size changes only when the keyboard's holder asks for a
// fit. An attach, typing and a window that resizes itself change nothing,
// the last said once; a fit is bounded like every size here, reaches the
// holder, and is told to everyone; and a fit after the CLI has ended is
// refused without anything falling over.
func TestAttachingAndTypingNeverResize(t *testing.T) {
	rec := filepath.Join(t.TempDir(), "record")
	p := withHeld(t, []string{"FAKE_HOLDER_RECORD=" + rec})
	id := p.start(map[string]any{"account": 1, "cols": 120, "rows": 40})["id"].(string)
	alice := p.attach(id, p.alice, "")
	if hello := alice.waitType("hello"); hello["cols"] != float64(120) || hello["rows"] != float64(40) {
		t.Fatalf("hello is %v", hello)
	}
	looker := p.attach(id, p.watch, "")
	looker.waitType("hello")
	alice.typeIn("hi\r")
	alice.waitOut("got:hi")

	alice.say(map[string]any{"type": "resize", "cols": 100, "rows": 30})
	alice.say(map[string]any{"type": "resize", "cols": 90, "rows": 20})
	alice.settled()
	said := 0
	for _, msg := range alice.errors() {
		if msg == heldNoFit {
			said++
		}
	}
	if said != 1 {
		t.Fatalf("a resize without fit was said %d times: %q", said, alice.errors())
	}
	if doc := p.describe(id); doc["cols"] != float64(120) || doc["rows"] != float64(40) {
		t.Fatalf("the size moved to %vx%v", doc["cols"], doc["rows"])
	}
	for _, line := range recorded(rec) {
		if strings.HasPrefix(line, "resize ") {
			t.Fatalf("the holder was asked to resize: %q", line)
		}
	}

	for _, c := range []struct {
		cols, rows int
		want       string
	}{{0, 0, "80x24"}, {5000, 5000, "1000x1000"}} {
		alice.say(map[string]any{"type": "resize", "cols": c.cols, "rows": c.rows, "fit": true})
		for _, tm := range []*term{alice, looker} {
			got := tm.waitType("resized")
			if size := strconv.Itoa(int(got["cols"].(float64))) + "x" + strconv.Itoa(int(got["rows"].(float64))); size != c.want {
				t.Fatalf("a fit to %dx%d was told as %s, want %s", c.cols, c.rows, size, c.want)
			}
		}
		waitRecorded(t, rec, "resize "+c.want)
	}

	// Only the holder fits.
	bob := p.attach(id, p.bob, "")
	bob.waitType("hello")
	bob.say(map[string]any{"type": "resize", "cols": 50, "rows": 10, "fit": true})
	if msg := bob.waitType("error")["message"]; msg != notHolding {
		t.Fatalf("a fit from somebody not holding the keyboard is refused with %q", msg)
	}

	// A fit once the CLI has gone is refused, and the server goes on.
	alice.typeIn("exit 5\r")
	alice.waitType("exit")
	p.waitEnded(id)
	again := p.attach(id, p.alice, "since=0")
	again.waitType("hello")
	again.say(map[string]any{"type": "resize", "cols": 70, "rows": 20, "fit": true})
	if msg := again.waitType("error")["message"]; msg != errTermEnded.Error() {
		t.Fatalf("a fit after the end is refused with %q", msg)
	}
	if doc := p.describe(id); doc["cols"] != float64(1000) {
		t.Fatalf("the size after the end is %v", doc["cols"])
	}
}

/* ------------------------------------------------------- falling behind --- */

// A viewer of a held session that cannot keep up is not closed: what it
// fell behind on is thrown away, and it is sent the screen as it is now
// and what came after that, after a repaint frame that says where its
// offsets go on from. The holder of the keyboard never notices.
//
// The holder's emulator here is half a megabyte behind its output, as one
// flooded is, so what comes after the screen is a lot: it comes in pieces,
// each written with its own deadline, never as one frame a slow viewer
// cannot finish.
func TestASlowViewerIsRepaintedNotDropped(t *testing.T) {
	was := termQueueFor
	termQueueFor = func(_ *Principal, watch bool) int {
		if watch {
			return 1
		}
		return 1 << 14
	}
	t.Cleanup(func() { termQueueFor = was })

	p := withHeld(t, []string{"FAKE_HOLDER_SCREEN_BEHIND=" + strconv.Itoa(512<<10)})
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	held := drain(alice.c)
	held.waitOut(t, "SCREEN")
	looker := p.attach(id, p.watch, "")
	p.waitViewers(id, 2)

	// Not read from while the session floods it with twenty megabytes, more
	// than the kernels on both ends buffer, so it falls behind for certain
	// rather than for luck. (A narrow socket would do it with less, but
	// reading that much back through a window of two kilobytes stalls on
	// linux.)
	alice.typeIn("flood 20000\r")
	held.waitOut(t, "flood done")
	// Somebody attaching meanwhile is a document, which is never thrown
	// away with the output.
	p.attach(id, p.bob, "mode=watch").waitType("hello")

	// Read as a page reads: offsets from the hello, a screen after each
	// repaint that is not output, and output adding up from the repaint's
	// offset. Caught up is the offset the session has got to.
	end := int64(p.describe(id)["offset"].(float64))
	at, screenNext, repaints := int64(0), false, 0
	for range 200000 {
		if repaints > 0 && !screenNext && at == end {
			break
		}
		op, payload := looker.step()
		switch op {
		case opClose:
			t.Fatalf("a slow viewer of a held session was closed with %d", closeCode(payload))
		case opText:
			var doc map[string]any
			_ = json.Unmarshal(payload, &doc)
			switch doc["type"] {
			case "hello":
				at, screenNext = int64(doc["offset"].(float64)), doc["repaint"] == true
			case "repaint":
				off, ok := doc["offset"].(float64)
				if !ok || doc["cols"] == nil || doc["rows"] == nil {
					t.Fatalf("the repaint frame is %v", doc)
				}
				at, screenNext = int64(off), true
				repaints++
			}
		case opBinary:
			if len(payload) > 2*termJoin {
				t.Fatalf("a slow viewer was sent %d bytes in one frame", len(payload))
			}
			if screenNext {
				if string(payload) != "SCREEN" {
					t.Fatalf("after a repaint frame came %q, not the screen", payload)
				}
				screenNext = false
				continue
			}
			at += int64(len(payload))
		}
	}
	if repaints == 0 || at != end {
		t.Fatalf("the slow viewer was repainted %d times and got to offset %d of %d", repaints, at, end)
	}
	looker.waitDoc("the viewers with bob", func(d map[string]any) bool {
		list, _ := d["list"].([]any)
		return d["type"] == "viewers" && len(list) == 3
	})
	// It is attached still, and in step with everybody else.
	alice.typeIn("after\r")
	held.waitOut(t, "got:after")
	looker.waitOut("got:after")
}

// A holder with no screen to give is asked twice, and then a viewer that
// fell behind is reset and replayed the end of the scrollback, after a gap
// for the rest: never the whole scrollback as one write, which a viewer slow
// enough to fall behind cannot finish before its deadline, to be dropped and
// come back for the same again. Every frame it is sent is a piece no bigger
// than a joined frame, and reading it as a page does gets to where the
// session is.
func TestASlowViewerWithoutAScreenIsRepaintedInPieces(t *testing.T) {
	was := termQueueFor
	termQueueFor = func(_ *Principal, watch bool) int {
		if watch {
			return 1
		}
		return 1 << 14
	}
	t.Cleanup(func() { termQueueFor = was })

	rec := filepath.Join(t.TempDir(), "record")
	p := withHeld(t, []string{"FAKE_HOLDER_SCREEN=none", "FAKE_HOLDER_RECORD=" + rec})
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	held := drain(alice.c)
	p.waitViewers(id, 1)
	looker := p.attach(id, p.watch, "")
	p.waitViewers(id, 2)
	alice.typeIn("flood 20000\r")
	held.waitOut(t, "flood done")

	end := int64(p.describe(id)["offset"].(float64))
	at, screenNext, repaints, gaps := int64(0), false, 0, 0
	for range 200000 {
		if repaints > 0 && !screenNext && at == end {
			break
		}
		op, payload := looker.step()
		switch op {
		case opClose:
			t.Fatalf("a slow viewer of a held session was closed with %d", closeCode(payload))
		case opText:
			var doc map[string]any
			_ = json.Unmarshal(payload, &doc)
			switch doc["type"] {
			case "hello":
				if doc["repaint"] == true {
					t.Fatalf("a holder with no screen gave one on attach: %v", doc)
				}
				at = int64(doc["offset"].(float64))
			case "repaint":
				at, screenNext = int64(doc["offset"].(float64)), true
				repaints++
			case "gap":
				to := int64(doc["to"].(float64))
				if screenNext || to < at {
					t.Fatalf("a gap at %d (screen still to come: %v) took the offset from %d to %d", at, screenNext, at, to)
				}
				at = to
				gaps++
			}
		case opBinary:
			if len(payload) > 2*termJoin {
				t.Fatalf("a slow viewer was sent %d bytes in one frame", len(payload))
			}
			if screenNext {
				if string(payload) != termReset {
					t.Fatalf("after a repaint frame with no screen came %q, not a reset", payload)
				}
				screenNext = false
				continue
			}
			at += int64(len(payload))
		}
	}
	if repaints == 0 || gaps == 0 || at != end {
		t.Fatalf("the slow viewer was repainted %d times with %d gaps and got to offset %d of %d", repaints, gaps, at, end)
	}
	asks := 0
	for _, line := range recorded(rec) {
		if line == "screen" {
			asks++
		}
	}
	// One for each attach, and two for each repaint.
	if asks < 2+2*repaints {
		t.Fatalf("the holder was asked for its screen %d times for 2 attaches and %d repaints", asks, repaints)
	}
	alice.typeIn("after\r")
	held.waitOut(t, "got:after")
	looker.waitOut("got:after")

	// Attaching now, with no screen to be had, replays the whole scrollback,
	// in pieces as well.
	late := p.attach(id, p.bob, "")
	from := int64(late.waitType("hello")["offset"].(float64))
	end, got := int64(p.describe(id)["offset"].(float64)), int64(0)
	for from+got < end {
		op, payload := late.step()
		if op == opClose {
			t.Fatalf("a late attach was closed with %d", closeCode(payload))
		}
		if op != opBinary {
			continue
		}
		if len(payload) > 2*termJoin {
			t.Fatalf("a late attach was replayed %d bytes in one frame", len(payload))
		}
		got += int64(len(payload))
	}
	if got < 1<<20 {
		t.Fatalf("a late attach was replayed %d bytes, not the scrollback", got)
	}
}

// A viewer that is behind when the CLI ends is still told how it ended, with
// no screen or output after that, and the socket closes as a finished
// terminal's does rather than as a slow reader's.
func TestASlowViewerIsToldTheExit(t *testing.T) {
	was := termQueueFor
	termQueueFor = func(_ *Principal, watch bool) int {
		if watch {
			return 1
		}
		return 1 << 14
	}
	t.Cleanup(func() { termQueueFor = was })

	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	held := drain(alice.c)
	held.waitOut(t, "SCREEN")
	looker := p.attach(id, p.watch, "")
	p.waitViewers(id, 2)
	alice.typeIn("flood 20000\r")
	held.waitOut(t, "flood done")
	alice.typeIn("exit 7\r")
	p.waitEnded(id)

	var exit map[string]any
	for range 100000 {
		op, payload, err := readFrame(looker.c)
		if err != nil {
			t.Fatalf("the slow viewer's socket broke before it closed: %v", err)
		}
		if op == opClose {
			if code := closeCode(payload); code != wsNormal {
				t.Fatalf("the slow viewer was closed with %d, want %d", code, wsNormal)
			}
			break
		}
		if op != opText {
			if exit != nil {
				t.Fatalf("output came after the exit: %q", payload)
			}
			continue
		}
		var doc map[string]any
		_ = json.Unmarshal(payload, &doc)
		switch {
		case exit != nil && doc["type"] == "repaint":
			t.Fatalf("a repaint came after the exit: %v", doc)
		case doc["type"] == "exit":
			exit = doc
		}
	}
	if exit == nil || exit["code"] != float64(7) {
		t.Fatalf("the slow viewer was told the exit %v, want code 7", exit)
	}
}
