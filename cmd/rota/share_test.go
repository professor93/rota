//go:build linux || darwin

package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/share"
)

// These are about the half of sharing that happens where the terminal is.
//
// What matters here is what a person sitting at that terminal would notice,
// and the answer has to be "nothing": their keystrokes reach the CLI, what
// it prints reaches their screen at the speed it was printed, and the modes
// they had are the modes they get back. A server that is slow, absent or
// misbehaving must not be able to change any of that, so the loops are
// written against interfaces and driven here by fakes that can be made to
// misbehave on purpose.

/* ---------------------------------------------------------------- fakes --- */

// held is a buffer two goroutines touch, which is every buffer in here: one
// loop writes it and the test reads it.
type held struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (h *held) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.b.Write(p)
}

func (h *held) String() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.b.String()
}

func (h *held) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.b.Len()
}

// fakeLocal is the window somebody is sitting at: what they are typing, what
// they are shown, and a record of every time the modes were changed.
type fakeLocal struct {
	in  io.Reader
	out held
	win chan struct{}

	mu       sync.Mutex
	rawTimes int
	restored int
	rawFails bool
}

func newFakeLocal(typing string) *fakeLocal {
	return &fakeLocal{in: strings.NewReader(typing), win: make(chan struct{}, 1)}
}

func (l *fakeLocal) Read(p []byte) (int, error)  { return l.in.Read(p) }
func (l *fakeLocal) Write(p []byte) (int, error) { return l.out.Write(p) }

func (l *fakeLocal) size() (uint16, uint16, error) { return 100, 30, nil }

func (l *fakeLocal) raw() (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.rawFails {
		return nil, errNotATerminal
	}
	l.rawTimes++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.restored++
	}, nil
}

func (l *fakeLocal) resized() <-chan struct{} { return l.win }

func (l *fakeLocal) modes() (raw, restored int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rawTimes, l.restored
}

// fakeChild is the CLI: what it prints, what it was typed, and how it ended.
type fakeChild struct {
	out  io.Reader
	in   held
	code int

	mu       sync.Mutex
	hangUps  int
	ends     int
	sizes    [][2]uint16
	waitFor  chan struct{}
	panicNow bool
}

func newFakeChild(prints string, code int) *fakeChild {
	return &fakeChild{out: strings.NewReader(prints), code: code}
}

func (c *fakeChild) Read(p []byte) (int, error) {
	n, err := c.out.Read(p)
	if err != nil && c.panicNow {
		panic("this child fell over")
	}
	return n, err
}

func (c *fakeChild) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *fakeChild) resize(cols, rows uint16) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes = append(c.sizes, [2]uint16{cols, rows})
	return nil
}

func (c *fakeChild) hangUp() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hangUps++
}

func (c *fakeChild) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ends++
}

func (c *fakeChild) wait() int {
	if c.waitFor != nil {
		<-c.waitFor
	}
	return c.code
}

func (c *fakeChild) close() {}

func (c *fakeChild) counted() (hangUps, ends int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hangUps, c.ends
}

// shorter makes the waits in here take no time, so a test about what happens
// after three seconds does not take three seconds.
func shorter(t *testing.T) {
	t.Helper()
	retry, beat, flush, kill := shareRetry, shareBeat, shareFlush, shareKillAfter
	shareRetry, shareBeat, shareFlush, shareKillAfter = 20*time.Millisecond, 50*time.Millisecond,
		100*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		shareRetry, shareBeat, shareFlush, shareKillAfter = retry, beat, flush, kill
	})
}

// noLink is a link pointed at a socket that will never be there, which is
// the ordinary case: most terminals are shared by somebody who has not
// started a server.
func noLink(t *testing.T) *shareLink {
	t.Helper()
	l := newShareLink(filepath.Join(t.TempDir(), "nobody.sock"), newShareQueue(1<<20), io.Discard,
		func() share.HelloMsg { return share.HelloMsg{Version: share.Version} })
	t.Cleanup(l.close)
	return l
}

/* ------------------------------------------------------------ the modes --- */

// The terminal is put back whatever happened to the CLI. This is the one
// thing in here that cannot be got wrong even once: a terminal left raw is
// one somebody has to blindly type `stty sane` into.
func TestTheTerminalIsPutBackWhenTheChildEnds(t *testing.T) {
	shorter(t)
	lo := newFakeLocal("")
	child := newFakeChild("hello from the CLI\r\n", 3)
	sh := &shareSession{lo: lo, child: child, link: noLink(t), hangs: make(chan os.Signal)}
	if code := sh.run(); code != 3 {
		t.Fatalf("the CLI's own exit code is rota's: %d", code)
	}
	raw, back := lo.modes()
	if raw != 1 || back != 1 {
		t.Fatalf("the modes were changed %d times and put back %d", raw, back)
	}
	if !strings.Contains(lo.out.String(), "hello from the CLI") {
		t.Fatalf("and the screen got everything: %q", lo.out.String())
	}
}

// And when the link misbehaves: a server that accepted the connection and
// then stopped reading a single byte is a link that blocks forever, and it
// changes nothing about the terminal or the way out of it.
func TestTheTerminalIsPutBackWhenTheLinkNeverReads(t *testing.T) {
	shorter(t)
	here, there := net.Pipe()
	defer here.Close()
	defer there.Close() // nothing ever reads "there", so every write blocks

	q := newShareQueue(4 << 10)
	l := newShareLink("", q, io.Discard, func() share.HelloMsg { return share.HelloMsg{} })
	defer l.close()
	stop := make(chan struct{})
	defer close(stop)
	go l.drain(share.NewConn(here, 0), stop)

	lo := newFakeLocal("")
	child := newFakeChild(strings.Repeat("a screenful of output\r\n", 4000), 0)
	sh := &shareSession{lo: lo, child: child, link: l, hangs: make(chan os.Signal)}
	done := make(chan int, 1)
	go func() { done <- sh.run() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a link that never reads held up the terminal it was sharing")
	}
	if _, back := lo.modes(); back != 1 {
		t.Fatalf("the terminal was not put back: %d", back)
	}
	if lo.out.Len() != len(strings.Repeat("a screenful of output\r\n", 4000)) {
		t.Fatalf("the screen was shown %d bytes of %d",
			lo.out.Len(), len(strings.Repeat("a screenful of output\r\n", 4000)))
	}
	if q.dropped() == 0 {
		t.Fatal("nothing was dropped for a server that read nothing, so something waited for it")
	}
}

// A panic below is still a terminal put back: the restore is deferred, and
// nothing between here and it calls os.Exit.
func TestTheTerminalIsPutBackThroughAPanic(t *testing.T) {
	shorter(t)
	lo := newFakeLocal("")
	child := newFakeChild("a line\r\n", 0)
	child.panicNow = true
	sh := &shareSession{lo: lo, child: child, link: noLink(t), hangs: make(chan os.Signal)}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the child was supposed to fall over")
			}
		}()
		sh.run()
	}()
	if _, back := lo.modes(); back != 1 {
		t.Fatalf("a panic left the terminal raw: restored %d times", back)
	}
}

// A terminal that was never a terminal is shared anyway, without a mode to
// put back. The refusal for that case belongs to the command, not here: this
// is only that nothing below it assumes raw mode worked.
func TestAWindowWithNoModesIsStillCopied(t *testing.T) {
	shorter(t)
	lo := newFakeLocal("")
	lo.rawFails = true
	child := newFakeChild("still printed\r\n", 0)
	sh := &shareSession{lo: lo, child: child, link: noLink(t), hangs: make(chan os.Signal)}
	sh.run()
	if !strings.Contains(lo.out.String(), "still printed") {
		t.Fatalf("the screen got nothing: %q", lo.out.String())
	}
}

// A signal to rota is passed on to the CLI and then insisted on, so that
// this process always reaches the restore rather than sitting behind a CLI
// that ignored the hangup.
func TestASignalToRotaHangsUpTheChild(t *testing.T) {
	shorter(t)
	lo := newFakeLocal("")
	child := newFakeChild("", 0)
	child.waitFor = make(chan struct{})
	hangs := make(chan os.Signal, 1)
	sh := &shareSession{lo: lo, child: child, link: noLink(t), hangs: hangs}
	go func() {
		hangs <- os.Interrupt
		time.Sleep(200 * time.Millisecond)
		close(child.waitFor)
	}()
	sh.run()
	hangUps, ends := child.counted()
	if hangUps != 1 || ends != 1 {
		t.Fatalf("a signal is a hangup and then the one nothing survives: %d and %d", hangUps, ends)
	}
	if _, back := lo.modes(); back != 1 {
		t.Fatalf("and the terminal is put back: %d", back)
	}
}

/* ------------------------------------------------------------- the queue --- */

// What the server misses is counted, and the count is handed over in front
// of the bytes that follow it — which is where it happened, because the
// oldest are the ones dropped.
func TestWhatTheServerMissedIsCountedAndAnnouncedInFrontOfTheRest(t *testing.T) {
	q := newShareQueue(1000)
	q.push(bytes.Repeat([]byte("a"), 600))
	q.push(bytes.Repeat([]byte("b"), 600))
	bits, lost, exit, ok := q.take(nil)
	if !ok || exit != nil {
		t.Fatalf("the queue had something to hand over: %v %v", ok, exit)
	}
	if lost != 600 {
		t.Fatalf("600 bytes went and %d were counted", lost)
	}
	if len(bits) != 1 || string(bits[0][:1]) != "b" {
		t.Fatalf("what is kept is the newest, which is what somebody watching wants: %d chunks", len(bits))
	}
	if q.dropped() != 0 {
		t.Fatal("a count that was handed over is not still owed")
	}
}

// And it reaches the server as a gap frame, before the output it precedes.
func TestAGapIsSentBeforeTheOutputThatFollowsIt(t *testing.T) {
	shorter(t)
	here, there := net.Pipe()
	defer here.Close()
	defer there.Close()
	q := newShareQueue(1000)
	q.push(bytes.Repeat([]byte("a"), 600))
	q.push(bytes.Repeat([]byte("b"), 600))
	l := newShareLink("", q, io.Discard, func() share.HelloMsg { return share.HelloMsg{} })
	defer l.close()
	stop := make(chan struct{})
	defer close(stop)
	go l.drain(share.NewConn(here, 0), stop)

	peer := share.NewConn(there, 0)
	peer.Deadline(time.Now().Add(10 * time.Second))
	kind, payload, err := peer.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kind != share.Gap {
		t.Fatalf("the first frame says what was missed, not what survived: %d", kind)
	}
	var gap share.GapMsg
	if share.Unmarshal(payload, &gap) != nil || gap.Bytes != 600 {
		t.Fatalf("and how much: %s", payload)
	}
	kind, payload, err = peer.Read()
	if err != nil || kind != share.Output || payload[0] != 'b' {
		t.Fatalf("and the output comes after it: %d %q %v", kind, payload, err)
	}
}

// The exit code travels behind the output rather than beside it, so a server
// is never told a terminal is over while the last of it is still queued.
func TestTheExitCodeGoesOutBehindTheOutput(t *testing.T) {
	shorter(t)
	here, there := net.Pipe()
	defer here.Close()
	defer there.Close()
	q := newShareQueue(1 << 20)
	q.push([]byte("the last line\r\n"))
	q.done(9)
	l := newShareLink("", q, io.Discard, func() share.HelloMsg { return share.HelloMsg{} })
	defer l.close()
	stop := make(chan struct{})
	defer close(stop)
	go l.drain(share.NewConn(here, 0), stop)

	peer := share.NewConn(there, 0)
	peer.Deadline(time.Now().Add(10 * time.Second))
	if kind, payload, err := peer.Read(); err != nil || kind != share.Output || !bytes.Contains(payload, []byte("last line")) {
		t.Fatalf("the output comes first: %d %q %v", kind, payload, err)
	}
	kind, payload, err := peer.Read()
	if err != nil || kind != share.Exit {
		t.Fatalf("and then the exit: %d %v", kind, err)
	}
	var msg share.ExitMsg
	if share.Unmarshal(payload, &msg) != nil || msg.Code != 9 {
		t.Fatalf("with the code the CLI ended with: %s", payload)
	}
}

/* -------------------------------------------------------------- the link --- */

// A server that is not running is not a problem: the terminal runs, and the
// link keeps trying quietly until there is one.
func TestALinkWaitsForAServerThatIsNotThereYet(t *testing.T) {
	shorter(t)
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")

	q := newShareQueue(1 << 20)
	l := newShareLink(path, q, io.Discard, func() share.HelloMsg {
		return share.HelloMsg{Version: share.Version, Label: "waiting"}
	})
	defer l.close()
	id, why := l.start()
	if id != "" || why != "" {
		t.Fatalf("nothing is listening, so there is nothing to say: %q %q", id, why)
	}

	// Now there is a server, and nothing had to be restarted for it.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	defer ln.Close()
	got := make(chan share.HelloMsg, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		c := share.NewConn(conn, 0)
		var hello share.HelloMsg
		if _, err := share.ReadJSON(c, &hello); err != nil {
			return
		}
		_ = c.SendJSON(share.Welcome, share.WelcomeMsg{Version: share.Version, Terminal: "t-1"})
		got <- hello
		// Held open so the link stays up while the test looks at it.
		time.Sleep(2 * time.Second)
		c.Close()
	}()
	select {
	case hello := <-got:
		if hello.Label != "waiting" {
			t.Fatalf("the hello is this terminal's: %+v", hello)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the link never came back to a server that had started")
	}
}

// A server that will not have this terminal says so in a sentence, and it is
// printed once rather than every three seconds on top of somebody's editor.
func TestARefusalIsPrintedOnceAndNotRetried(t *testing.T) {
	shorter(t)
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("no unix socket here: %v", err)
	}
	defer ln.Close()
	tries := make(chan struct{}, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			c := share.NewConn(conn, 0)
			var hello share.HelloMsg
			if _, err := share.ReadJSON(c, &hello); err == nil {
				_ = c.SendJSON(share.Refused, share.RefusedMsg{
					Message: "this rota speaks another version", Final: true})
			}
			c.Close()
			select {
			case tries <- struct{}{}:
			default:
			}
		}
	}()

	var said held
	l := newShareLink(path, newShareQueue(1<<20), &said, func() share.HelloMsg {
		return share.HelloMsg{Version: share.Version}
	})
	defer l.close()
	id, why := l.start()
	if id != "" || why != "this rota speaks another version" {
		t.Fatalf("the refusal comes back as it was written: %q %q", id, why)
	}
	<-tries
	time.Sleep(200 * time.Millisecond) // many retries' worth
	if len(tries) > 0 {
		t.Fatal("a refusal that said not to come back was come back from")
	}
	if said.Len() != 0 {
		t.Fatalf("the first refusal is the caller's to print, not the link's: %q", said.String())
	}
}

/* ------------------------------------------------------------- the flags --- */

// --share is taken out of what was typed, and nothing else is.
func TestWhatShareTakesOutOfTheCommandLine(t *testing.T) {
	for _, c := range []struct {
		in    []string
		rest  string
		on    bool
		mode  string
		label string
	}{
		{in: []string{}, rest: ""},
		{in: []string{"--share"}, rest: "", on: true, mode: "control"},
		{in: []string{"-i", "--share"}, rest: "-i", on: true, mode: "control"},
		{in: []string{"--share=watch"}, rest: "", on: true, mode: "watch"},
		{in: []string{"--share=control", "--label", "api"}, rest: "", on: true, mode: "control", label: "api"},
		{in: []string{"--label=api", "--share"}, rest: "", on: true, mode: "control", label: "api"},
		// Everything after -- belongs to the CLI, --share included.
		{in: []string{"--", "--share"}, rest: "-- --share"},
		{in: []string{"--share", "--", "--share"}, rest: "-- --share", on: true, mode: "control"},
		// A prompt that happens to contain the word is a prompt.
		{in: []string{"tell me about --share"}, rest: "tell me about --share"},
	} {
		rest, ask, err := takeShareFlags(c.in)
		if err != nil {
			t.Fatalf("%v: %v", c.in, err)
		}
		if got := strings.Join(rest, " "); got != c.rest {
			t.Errorf("%v left %q, want %q", c.in, got, c.rest)
		}
		if ask.on != c.on || ask.mode != c.mode || ask.label != c.label {
			t.Errorf("%v asked for %+v, want on=%v mode=%q label=%q", c.in, ask, c.on, c.mode, c.label)
		}
	}
	if _, _, err := takeShareFlags([]string{"--share=sometimes"}); err == nil {
		t.Fatal("a mode nobody wrote is refused")
	} else if !strings.Contains(err.Error(), shareBadMode) {
		t.Fatalf("and says what the two are: %v", err)
	}
}

// The name a shared terminal gets when nobody said: the folder's, which is
// what somebody recognizes it by in a list.
func TestATerminalIsNamedAfterItsFolder(t *testing.T) {
	if got := shareLabel("/src/fintech/api"); got != "api" {
		t.Fatalf("the folder's name is the default: %q", got)
	}
	if got := shareLabel("/"); got != "" {
		t.Fatalf("and the root has none worth showing: %q", got)
	}
}

// The one line rota prints before the CLI takes the screen, in each of the
// three things it can be.
func TestTheOpeningLineSaysWhichOfTheThreeItIs(t *testing.T) {
	linked := shareOpening("1 a@x", "claude", "t-9", "/tmp/s.sock", share.ModeControl)
	if !strings.Contains(linked, "shared to the rota server on this machine as terminal t-9") {
		t.Fatalf("a terminal that is shared says where: %q", linked)
	}
	watched := shareOpening("1 a@x", "claude", "t-9", "/tmp/s.sock", share.ModeWatch)
	if !strings.Contains(watched, "to be watched only") {
		t.Fatalf("and whether it may be typed into: %q", watched)
	}
	alone := shareOpening("1 a@x", "claude", "", "/tmp/s.sock", share.ModeControl)
	if !strings.Contains(alone, "no rota server is listening") || !strings.Contains(alone, "sharing begins when one is") {
		t.Fatalf("a terminal that is not says so, and that it will be: %q", alone)
	}
	long := shareOpening("1 a@x", "claude", "", strings.Repeat("d/", 80)+"s.sock", share.ModeControl)
	if !strings.Contains(long, "too long for a unix socket") {
		t.Fatalf("and a path no socket can have says that instead: %q", long)
	}
}
