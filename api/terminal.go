package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	rota "github.com/professor93/rota/lib"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/ring"
	"github.com/professor93/rota/internal/vtscan"
)

// A terminal is an account's CLI on a pseudo-terminal that lives in this
// server rather than in a browser tab: it is started once, it keeps running
// when the tab closes, and anyone allowed to may attach to it and watch what
// it prints — or, if they hold the keyboard, type into it.
//
// It is the same shape as a live run, deliberately: a ring of what has been
// printed with a replay for whoever attaches, a grace period rather than an
// instant death, one fan-out to several readers. It differs in the two ways
// a terminal is not a run. What it carries is bytes, not events, so the ring
// is a byte ring and the offsets are absolute; and it takes input from the
// one connection that holds the keyboard, so that a watcher shown the page
// is a watcher in fact and not by the page's good manners.
//
// Nothing here is on unless the configuration says so. Whoever can reach
// this with a control sign-in can run commands on this machine as the user
// the server runs as, which is why the group is off by default and why a
// server listening anywhere but the loopback is refused without TLS.

const (
	// termKindAccount is a terminal running an account's vendor CLI,
	// termKindShell one running the person's login shell — which is off
	// unless the file asks for it — and termKindShared one that is not this
	// server's at all: a terminal somebody is sitting at, offered to this
	// server by the rota that is running it.
	termKindAccount = "account"
	termKindShell   = "shell"
	termKindShared  = "shared"

	// termQueue is how many frames one connection may fall behind by. A
	// terminal fans out to everyone attached, and a reader that has stopped
	// reading must not be able to hold up the terminal or anybody else: past
	// this it is closed and told to come back with an offset.
	termQueue = 256

	// termJoin is how much output one frame may grow to by taking in what is
	// queued behind it. It is a burst rather than a screen: past this the
	// frame goes out and the rest waits for the next one.
	termJoin = 64 << 10

	// termRead is one read of the master. It is the size of a burst of
	// output rather than of a line: a program redrawing a full screen prints
	// tens of kilobytes at once.
	termRead = 32 << 10

	// wsTooSlow is the close code for a reader that could not keep up. 1013
	// is "try again later", which is exactly the case: the connection is
	// closed, the terminal is fine, and reattaching with since picks up
	// everything that was missed.
	wsTooSlow = 1013

	// termDocRoom is how many documents a held session's viewer may have
	// queued beyond its output. Its output may fall behind and be repainted,
	// but a keyboard change or the exit is not output, and has to arrive.
	termDocRoom = 64
	// termLate bounds the documents a viewer that fell behind keeps for
	// after its repaint. A viewer owed more than that is closed with 1013,
	// as any other terminal's reader is: a document lost is not said again.
	termLate = 1024
	// termReset is the repaint of a held session whose holder has no screen
	// to give: a reset, and then the last termTail of what the ring holds,
	// with a gap for the rest.
	termReset = "\x1bc"
	// termTail is how much output a repaint without a screen replays. A
	// viewer that fell behind is slow, and the whole scrollback is more than
	// a slow viewer takes in before it falls behind again.
	termTail = 64 << 10
)

var (
	// keyboardGrace is how long a dropped holder keeps the keyboard, so that
	// a reconnect — a laptop lid, a proxy, a refreshed tab — gets it back
	// rather than finding somebody else typing.
	//
	// claimWait is how long a claim may go unanswered before it may be
	// forced, and termKillAfter how long a terminal has to end on a hangup
	// before it is killed outright. All three are variables so a test about
	// what happens after them need not take that long.
	keyboardGrace = 5 * time.Second
	claimWait     = 10 * time.Second
	termKillAfter = 3 * time.Second
	// termSweep is how often terminals nobody is attached to are measured
	// against the idle timeout.
	termSweep = 30 * time.Second
	// termRepaintGap is the least time between two repaints of one held
	// session's viewer. A viewer that stays slow falls behind again soon
	// after each, and each asks the holder for a screen.
	termRepaintGap = 250 * time.Millisecond
)

// termQueueFor is how deep one connection's queue is, given who is behind it
// and whether it attached to watch.
//
// It is a variable, and it takes the connection rather than nothing, so that
// a test about a reader which cannot keep up can make exactly that reader
// shallow. A knob that made every queue shallow would make such a test about
// how promptly the test itself gets round to reading, and the reader it
// needs to go on working would be the one dropped.
var termQueueFor = func(*Principal, bool) int { return termQueue }

// The refusals a connection is given for trying to type, or to change a
// terminal that is not this server's to change. They are constants because
// they are part of what this server answers, and a test reads them.
const (
	notHolding = "you are not holding the keyboard"
	watchConn  = "this connection attached to watch; attach again with mode=control to ask for the keyboard"
	// sharedWatch is a terminal offered to be read and never typed into. It
	// is the sharer's decision, made where the terminal is, so no role on
	// this server overrides it.
	sharedWatch = "this terminal is shared to be watched only"
	// sharedSize is the other thing a shared terminal does not take: its
	// size belongs to the window somebody is actually sitting at.
	sharedSize = "this terminal's size follows the window it was started in"
	// heldNoFit is said once to a viewer of a held session that asks for a
	// size without asking to fit: a window that resized itself, a page
	// fitting itself on attach. A held session is looked at from devices of
	// every size, and only an explicit fit by the keyboard's holder changes
	// it, so a stray tap on a phone does not reflow the desk.
	heldNoFit = "this session's size changes only when a viewer asks to fit it"
	// sendFirst is somebody typing into a held session while another
	// person's send waits there to be typed, naming who sent it. It is said
	// to each connection at most once every sendFirstEvery, and keystrokes
	// refused in between are refused without a word (errSendTold).
	sendFirst = "a send from %s is waiting to be typed into this session; type once it is in"
)

// sendFirstEvery is the least time between two sendFirst said to one
// connection whose keystrokes go on being refused: often enough that a key
// never vanishes for long without a reason, and seldom enough that a typist
// on a slow link is not sent so many refusals that it falls behind.
const sendFirstEvery = 2 * time.Second

var (
	errNotHolding = errors.New(notHolding)
	// errSendTold is a keystroke refused under a waiting send less than
	// sendFirstEvery after the connection was told of one: nothing is typed
	// and nothing more is said, so a typist on a slow link is not sent a
	// refusal for every key.
	errSendTold    = errors.New("refused under a waiting send this connection was just told of")
	errTermEnded   = errors.New("this terminal has ended")
	errSharedWatch = errors.New(sharedWatch)
	errSharedSize  = errors.New(sharedSize)
)

/* -------------------------------------------------------------- the clock --- */

// clock is the time terminals are measured against. It is behind a lock
// because a test moves it while sockets are reading it, and a field set from
// one goroutine and read by another is a race however harmless it looks.
type clock struct {
	mu sync.Mutex
	at func() time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		return time.Now()
	}
	return c.at()
}

func (c *clock) set(at func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

func (s *Server) now() time.Time { return s.clock.now() }

/* ----------------------------------------------------------- a connection --- */

// termFrame is one thing the server sends a connection: output as bytes, or
// a document as text. Both go through one queue so they keep their order —
// a keyboard change that overtook the output it explains would read as a
// lie.
type termFrame struct {
	op   byte
	data []byte
	// alone is output that goes out as a frame of its own rather than being
	// joined to what is queued around it: a held session's repaint, which a
	// viewer must be able to tell from the bytes that follow it.
	alone bool
	// late is a document queued for a held session's viewer while it was
	// behind: it is sent after the repaint, not before, so the exit frame
	// is never followed by the screen it ended on.
	late bool
	// covered is a document about the output, a gap, which a held session's
	// viewer that is behind is not sent: its repaint goes on from past it,
	// and the gap's offsets after that would take it backwards.
	covered bool
}

// termConn is one attached socket: who is behind it, whether it may ever
// type, and the bounded queue everything it is sent goes through.
type termConn struct {
	id    string
	name  string
	role  Role
	watch bool // this connection may never hold the keyboard
	// refusal is the sentence a change this role does not cover is refused
	// in, which depends on who the principal is (watchOnlyFor).
	refusal string
	since   time.Time

	c    *wsConn
	out  chan termFrame
	gone chan struct{}
	// drained closes once the writing goroutine has finished with the
	// queue, so nothing says goodbye over the top of a frame still waiting
	// to go out.
	drained chan struct{}
	once    sync.Once
	// pending is a document that turned up while output was being joined
	// together, kept to be sent next so nothing overtakes anything. It
	// belongs to the writing goroutine alone.
	pending *termFrame
	// slow says this connection was closed for falling behind, so the close
	// frame can say which of the two it was.
	slow atomic.Bool
	// skip is the offset this connection's output resumes at, when it was
	// shown a held session's screen standing for bytes this server had yet
	// to be sent: those are on its screen already. Zero is nothing to skip.
	// It belongs to the session's mu.
	skip int64

	// The rest is a held session's viewer's alone (repainting), and stays
	// empty for every other terminal, whose rules do not change.
	//
	// person is who this connection is for the keyboard (personOf): the
	// keyboard follows a person across their connections. "" is nobody.
	person string
	// replies takes out of what this connection types the answers its own
	// terminal gives to the queries it sees replayed, which the holder
	// answered already. noFit is heldNoFit having been said. Both belong to
	// the goroutine reading the connection.
	replies *vtscan.ReplyFilter
	noFit   bool
	// toldSendAt is when this connection was last told a send is waiting
	// (sendFirst), so that it is told at most once every sendFirstEvery. It
	// belongs to the session's mu.
	toldSendAt time.Time
	// catchUp is what a viewer that fell behind is sent instead of being
	// closed with 1013. depth is how many frames of output it may fall
	// behind by, and queued how many of them are in its queue. behind is it
	// having fallen behind: its output is thrown away until it is
	// repainted, which shows all of it. repaintAt is the soonest the next
	// repaint may go, and belongs to the writing goroutine.
	catchUp   func(*termConn) []termFrame
	depth     int32
	queued    atomic.Int32
	behind    atomic.Bool
	repaintAt time.Time
}

func newTermConn(c *wsConn, p *Principal, watch bool, at time.Time) *termConn {
	depth := termQueueFor(p, watch)
	if depth < 1 {
		depth = 1
	}
	return &termConn{
		id: runID(), name: p.Name, role: p.Role, watch: watch, since: at, refusal: watchOnlyFor(p),
		c: c, out: make(chan termFrame, depth),
		gone: make(chan struct{}), drained: make(chan struct{}),
	}
}

// repainting makes a connection a held session's viewer, before it is
// attached: it types as person, its terminal's answers are taken out of
// what it types, and falling behind repaints it rather than closing it.
// Its queue gets room for documents beyond the output it may fall behind
// by.
func (tc *termConn) repainting(person string, catchUp func(*termConn) []termFrame) {
	tc.person, tc.replies, tc.catchUp = person, &vtscan.ReplyFilter{}, catchUp
	tc.depth = int32(cap(tc.out))
	tc.out = make(chan termFrame, cap(tc.out)+termDocRoom)
}

// push queues one frame, and closes the connection rather than waiting when
// the queue is full. That is the whole rule that keeps one slow watcher from
// stalling the terminal: the terminal never blocks on a reader.
func (tc *termConn) push(op byte, data []byte) { tc.queue(termFrame{op: op, data: data}) }

func (tc *termConn) queue(f termFrame) {
	if tc.catchUp != nil {
		tc.queueHeld(f)
		return
	}
	select {
	case tc.out <- f:
	default:
		tc.slow.Store(true)
		tc.stop()
	}
}

// queueHeld is queue for a held session's viewer, which is not closed for
// falling behind on output. Output past its depth marks it behind, and from
// then on its output is thrown away: the repaint it is sent once its queue
// has drained shows all of it. A document still goes in, in the room kept
// for them, marked late when it came while the viewer was behind — except a
// gap, which the repaint goes on from past.
//
// A document is not output, and no repaint says it again, so one that finds
// even that room full closes the viewer with 1013 after all: reattaching is
// told in its hello what it would have missed.
func (tc *termConn) queueHeld(f termFrame) {
	behind := tc.behind.Load()
	if f.op == opBinary {
		if behind {
			return
		}
		if tc.queued.Load() >= tc.depth {
			tc.behind.Store(true)
			return
		}
		tc.queued.Add(1)
	} else {
		if behind && f.covered {
			return
		}
		f.late = behind
	}
	select {
	case tc.out <- f:
	default:
		if f.op == opBinary {
			tc.queued.Add(-1)
			tc.behind.Store(true)
			return
		}
		tc.slow.Store(true)
		tc.stop()
	}
}

// pushOutput queues output. A held session's viewer is given it in pieces
// no bigger than a joined frame, so a replay of the whole scrollback is many
// writes, each with its own deadline, rather than one a slow viewer cannot
// finish in time and is dropped for, to come back for the same again.
func (tc *termConn) pushOutput(p []byte) {
	if tc.catchUp == nil {
		tc.push(opBinary, p)
		return
	}
	for _, piece := range pieces(p) {
		tc.push(opBinary, piece)
	}
}

// pieces cuts output into frames of at most termJoin.
func pieces(p []byte) [][]byte {
	var out [][]byte
	for len(p) > 0 {
		n := min(len(p), termJoin)
		out, p = append(out, p[:n:n]), p[n:]
	}
	return out
}

// took is one frame taken off the queue by the writing goroutine.
func (tc *termConn) took(f termFrame) {
	if f.op == opBinary && tc.catchUp != nil {
		tc.queued.Add(-1)
	}
}

func (tc *termConn) say(v any) {
	raw, err := rota.Encode(v)
	if err != nil {
		return
	}
	tc.push(opText, raw)
}

func (tc *termConn) fail(msg string) {
	tc.say(map[string]any{"type": "error", "message": msg})
}

func (tc *termConn) stop() { tc.once.Do(func() { close(tc.gone) }) }

// done reports whether this connection has been finished with.
func (tc *termConn) done() bool {
	select {
	case <-tc.gone:
		return true
	default:
		return false
	}
}

// write is this connection's one writing goroutine: everything the terminal
// says to it, in order, on the socket. When the connection is finished it
// drains what is already queued first, so the exit frame that was pushed
// just before the close still arrives.
//
// A held session's viewer that fell behind has the output in its queue
// thrown away and the documents queued before it fell behind sent, and once
// the queue is empty, and termRepaintGap after its last repaint, it is
// repainted, and then sent the documents that came while it was behind.
func (tc *termConn) write() {
	defer close(tc.drained)
	var late []termFrame
	for {
		var f termFrame
		switch {
		case tc.pending != nil:
			f, tc.pending = *tc.pending, nil
		case tc.behind.Load() && len(tc.out) == 0 && !tc.done() && !time.Now().Before(tc.repaintAt):
			if !tc.repaint(late) {
				tc.stop()
				return
			}
			late, tc.repaintAt = nil, time.Now().Add(termRepaintGap)
			continue
		default:
			var got, gone bool
			if f, got, gone = tc.next(); gone {
				tc.goodbye(late)
				return
			} else if !got {
				continue // the next repaint may go
			}
		}
		if tc.behind.Load() {
			switch {
			case f.op == opBinary:
			case f.late:
				if len(late) == termLate {
					tc.slow.Store(true)
					tc.stop()
					return
				}
				late = append(late, f)
			default:
				if err := tc.c.send(f.op, f.data); err != nil {
					tc.stop()
					return
				}
			}
			continue
		}
		if f.op == opBinary && !f.alone {
			f.data = tc.join(f.data)
		}
		if err := tc.c.send(f.op, f.data); err != nil {
			tc.stop()
			return
		}
	}
}

// next is the next frame off the queue, waiting for one as long as it
// takes, or, for a viewer that is behind, only until its next repaint may
// go: got is false then. gone is the connection having been finished with.
func (tc *termConn) next() (f termFrame, got, gone bool) {
	var soon <-chan time.Time
	if wait := time.Until(tc.repaintAt); wait > 0 && tc.behind.Load() {
		t := time.NewTimer(wait)
		defer t.Stop()
		soon = t.C
	}
	select {
	case f = <-tc.out:
		tc.took(f)
		return f, true, false
	case <-tc.gone:
		return f, false, true
	case <-soon:
		return f, false, false
	}
}

// goodbye sends what is still queued once the connection is finished, so
// the exit frame that was pushed just before the close still arrives. A
// viewer that is behind is sent its documents and no output: there is no
// repaint on the way out.
func (tc *termConn) goodbye(late []termFrame) {
	for {
		select {
		case f := <-tc.out:
			tc.took(f)
			behind := tc.behind.Load()
			switch {
			case behind && f.op == opBinary:
				continue
			case behind && f.late:
				late = append(late, f)
				continue
			}
			if err := tc.c.send(f.op, f.data); err != nil {
				return
			}
		default:
			for _, f := range late {
				if err := tc.c.send(f.op, f.data); err != nil {
					return
				}
			}
			return
		}
	}
}

// repaint brings a viewer that fell behind back to now: what its session's
// catchUp gives, and then the documents that waited for it.
func (tc *termConn) repaint(late []termFrame) bool {
	for _, f := range append(tc.catchUp(tc), late...) {
		if err := tc.c.send(f.op, f.data); err != nil {
			return false
		}
	}
	return true
}

// settle ends this connection and waits for what is already queued to have
// gone out, so a goodbye never overtakes the frame it is the goodbye for —
// the exit frame, most of all. The wait is bounded by how long one write may
// take: a peer that has stopped reading altogether must not hold up the
// goroutine that is saying goodbye to it.
func (tc *termConn) settle() {
	tc.stop()
	select {
	case <-tc.drained:
	case <-time.After(wsWriteWait):
	}
}

// join makes one frame of the output already queued behind this one.
//
// A terminal's output is a stream of bytes and not a sequence of messages,
// so nothing is lost by putting several together — and a pseudo-terminal
// hands its output over a line at a time, so a CLI drawing a screen would
// otherwise be thousands of frames, each with its own write. A document
// found on the way keeps its place: it is put aside and sent next, because
// a keyboard change that overtook the output it explains would read as a
// lie.
func (tc *termConn) join(first []byte) []byte {
	out := first
	for len(out) < termJoin {
		select {
		case f := <-tc.out:
			tc.took(f)
			if f.op != opBinary || f.alone {
				tc.pending = &f
				return out
			}
			// A new slice every time: the frame this started from is the
			// same one every other connection was given.
			joined := make([]byte, 0, len(out)+len(f.data))
			out = append(append(joined, out...), f.data...)
		default:
			return out
		}
	}
	return out
}

/* ------------------------------------------------------- the other end --- */

// termBackend is whatever is at the far end of a terminal.
//
// For most of them it is a process on a pseudo-terminal this server started,
// and for a shared one it is another rota on a socket, with the process at
// the far end of that. Everything above this line — the ring with its
// absolute offsets, the fan-out, the slow reader closed with 1013, the
// replay, the keyboard, the audit, the recording, the listing — is the same
// either way, and says so by never mentioning a file descriptor.
//
// What a terminal needs of its other end is small: take these bytes, be this
// size, stop, and print until there is no more.
type termBackend interface {
	// typeInto writes the bytes of somebody typing. An error is the answer
	// the connection that typed them is given.
	typeInto(p []byte) error
	// resize tells it the window is a different size, or says why it will
	// not be told — a shared terminal's size is not this server's to set.
	resize(cols, rows uint16) error
	// hangUp is the ending a CLI is written to tidy up after, and end is
	// the one nothing survives, a few seconds later.
	hangUp()
	end()
	// run carries everything printed into sink until there is nothing more,
	// and answers with the exit code. It is the whole life of the far end,
	// and returning from it is what ends the session.
	run(sink termSink) int
	// close lets go of what it holds, once the session is finished with it.
	close()
	// detach lets go of a far end that goes on without this server, and
	// makes run return without an ending. Only a held terminal is one; for
	// the others it does nothing.
	detach()
	// describe adds what only this kind of far end knows to a terminal's
	// description.
	describe(doc map[string]any)
}

// termSink is the session as its other end sees it: somewhere to put output,
// and the two things only a shared far end ever says.
type termSink interface {
	output(p []byte)
	resizedBy(cols, rows uint16)
	gapOf(bytes int64)
}

// localTerm is the far end this server started itself: a process on a
// pseudo-terminal of its own, which is what every terminal was before one
// could be shared.
type localTerm struct {
	master *os.File
	cmd    *exec.Cmd
	proc   *os.Process
}

func (l *localTerm) typeInto(p []byte) error { _, err := l.master.Write(p); return err }

func (l *localTerm) resize(cols, rows uint16) error { return pty.Resize(l.master, cols, rows) }

func (l *localTerm) hangUp() {
	if l.proc != nil {
		_ = l.proc.Signal(syscall.SIGHUP)
	}
}

func (l *localTerm) end() {
	if l.proc != nil {
		_ = l.proc.Signal(syscall.SIGKILL)
	}
}

func (l *localTerm) close() { l.master.Close() }

func (l *localTerm) detach() {}

func (l *localTerm) describe(map[string]any) {}

// run is the output pump and then the wait.
//
// The order here is fixed, and it is deliberately not the order in which the
// two things that end a terminal are noticed. On Linux a read of the master
// fails the moment the child's side of it is closed, while on macOS it hands
// back what is left and then reports the end, so "the output has stopped"
// and "the process has exited" arrive in either order depending on the
// platform. Everything that follows from the ending happens after both.
//
// Any read error ends it, and none of them is worth a line in the log: a
// master whose child has gone reads as EIO on Linux and as an end of file on
// macOS, and both mean the same thing. Whatever came back with the error is
// kept first, because a read may hand over the last of the output and the
// end of it at the same time.
func (l *localTerm) run(sink termSink) int {
	buf := make([]byte, termRead)
	for {
		n, err := l.master.Read(buf)
		if n > 0 {
			sink.output(buf[:n])
		}
		if err != nil {
			break
		}
	}
	_ = l.cmd.Wait() // and only then is there an exit code to report
	if l.cmd.ProcessState != nil {
		return l.cmd.ProcessState.ExitCode()
	}
	return -1
}

/* -------------------------------------------------------------- a session --- */

// termClaim is somebody asking for the keyboard: the connection that asked
// and when it first did, which is what force is measured against.
type termClaim struct {
	conn *termConn
	at   time.Time
}

// termSession is one terminal: what is running, what it has printed, who is
// attached, and who may type.
type termSession struct {
	s   *Server
	id  string
	log func(msg string, args ...any)

	kind     string
	account  int
	label    string // the account's own name
	provider string
	name     string // what the person who started it called it
	cwd      string
	started  time.Time
	// tokenUntil is when the login inside this terminal lapses: the stored
	// login's own end for a claude terminal on it, the token's expiry for one
	// on a token (see loginUntil). A person looking at a terminal that has
	// been open for a day wants to know that before the CLI tells them.
	tokenUntil time.Time

	back    termBackend
	release func()
	rec     *recorder
	// lookOnly is a terminal shared to be watched: nobody on this server may
	// type into it or hold its keyboard, whatever their role. It is set once
	// when the session is made and never changes.
	lookOnly bool

	mu         sync.Mutex
	cols, rows uint16
	ring       *ring.Ring
	conns      []*termConn
	holder     *termConn
	// holderName and freeAt are the keyboard's grace: whose it still is
	// after their socket dropped, and until when. holderPerson is the
	// person holding it (personOf), on a held session, kept through the
	// grace; "" on any other.
	holderName   string
	holderPerson string
	freeAt       time.Time
	claims       []*termClaim
	ended        bool
	exit         int
	// idleAt is when the last connection left, and zero while somebody is
	// attached. The idle timeout is measured from it.
	idleAt  time.Time
	killing bool
	// left is a held terminal this server has let go of, still running: it
	// is the next daemon's, and nothing here ends it any more.
	left bool
}

/* ------------------------------------------------------------ the keyboard --- */

// holds is the keyboard rule itself, and the only statement of it: this
// exact connection is the holder, it is one that may ever hold the keyboard,
// and the principal behind it may control. Identity and role, not the page's
// good manners.
//
// A terminal shared to be watched has no keyboard at all, so nobody holds
// it: the person at the far end is typing at their own machine, and what
// they offered this server was a window onto it.
//
// Called with mu held.
func (ts *termSession) holds(tc *termConn) bool {
	return tc != nil && !ts.lookOnly && ts.holder == tc && !tc.watch && tc.role.allows(RoleControl)
}

// typeInto is the one door into the terminal's input. Every byte a client
// sends passes through here and nowhere else, which is what makes the
// keyboard a fact about this server rather than a convention of the page.
func (ts *termSession) typeInto(tc *termConn, p []byte) error {
	ts.mu.Lock()
	if ts.ended {
		ts.mu.Unlock()
		return errTermEnded
	}
	if ts.lookOnly {
		ts.mu.Unlock()
		return errSharedWatch
	}
	holds := ts.holds(tc)
	if !holds && !ts.follows(tc) {
		ts.mu.Unlock()
		return errNotHolding
	}
	h, held := ts.back.(*heldTerm)
	if held {
		// A send waiting to be typed was let in under the keyboard's rule,
		// and holds the session for its person until it is in: somebody
		// else's keystrokes would be typed into the middle of it, and its
		// Enter would submit them. One Esc or one Ctrl-C is let through,
		// because a person must always be able to stop a turn, and the send
		// goes on waiting.
		if by, ok := h.sendingFor(tc.person, interrupts(p)); ok {
			now := ts.s.now()
			say := tc.toldSendAt.IsZero() || now.Sub(tc.toldSendAt) >= sendFirstEvery
			if say {
				tc.toldSendAt = now
			}
			ts.mu.Unlock()
			if !say {
				return errSendTold
			}
			return fmt.Errorf(sendFirst, by)
		}
	}
	if !holds {
		ts.move(tc)
	}
	back := ts.back
	ts.mu.Unlock()
	if held {
		// By connection, so a refusal that comes back later finds who typed.
		return h.typeFrom(tc, p)
	}
	return back.typeInto(p)
}

// interrupts reports whether a frame is one Esc or one Ctrl-C and nothing
// else: somebody stopping a turn, which a waiting send lets through once.
// Two of them may do more than stop it — a second Ctrl-C quits Claude Code,
// and a second Esc opens its rewind picker — an arrow key or history recall
// begins with Esc but carries more, and Ctrl-D may end the CLI, so none of
// those is one.
func interrupts(p []byte) bool {
	return len(p) == 1 && (p[0] == 0x1b || p[0] == 0x03)
}

// resizeTo is the other thing only the holder may do: the size is the
// terminal's, not the viewer's, and a watcher whose window is narrower must
// not reflow everybody else's screen.
//
// The far end has the last word on it. A shared terminal is a window
// somebody is sitting at, and a page cannot reach across and make that
// window a different size, so it is refused and nothing changes here.
func (ts *termSession) resizeTo(tc *termConn, cols, rows uint16) error {
	cols, rows = boundSize(cols, rows)
	ts.mu.Lock()
	if ts.ended {
		ts.mu.Unlock()
		return errTermEnded
	}
	if ts.lookOnly {
		ts.mu.Unlock()
		return errSharedWatch
	}
	if !ts.holds(tc) {
		ts.mu.Unlock()
		return errNotHolding
	}
	back := ts.back
	ts.mu.Unlock()
	if err := back.resize(cols, rows); err != nil {
		return err
	}
	// A holder says the size once it has taken it, and everyone is told
	// then: one whose CLI is ending, or whose terminal refused the size,
	// leaves it as it was.
	if !ts.held() {
		ts.resizedBy(cols, rows)
	}
	return nil
}

// resizedBy is the size having changed, wherever the change came from: this
// server having set it, or a sharer saying its window was dragged. Everyone
// attached is told, because everyone attached is drawing that size.
func (ts *termSession) resizedBy(cols, rows uint16) {
	cols, rows = boundSize(cols, rows)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.ended || (ts.cols == cols && ts.rows == rows) {
		return
	}
	ts.cols, ts.rows = cols, rows
	ts.tell(map[string]any{"type": "resized", "cols": cols, "rows": rows})
}

// gapOf is output that never reached this server: a sharer's queue overflowed
// because this server was slow or was not there at all. It is said rather
// than papered over, in the same frame and the same words a reader gets when
// it asks for an offset the scrollback no longer has — the offsets stay
// consistent either way, because the bytes were never counted.
func (ts *termSession) gapOf(n int64) {
	if n <= 0 {
		return
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.ended {
		return
	}
	at := ts.ring.End()
	ts.tellGap(map[string]any{"type": "gap", "from": at, "to": at, "bytes": n})
}

// free reports whether the keyboard is nobody's, letting a lapsed grace go
// on the way past and saying so when it does. Called with mu held.
func (ts *termSession) free() bool {
	if ts.holder != nil {
		return false
	}
	if ts.holderName == "" {
		return true
	}
	if ts.s.now().Before(ts.freeAt) {
		return false
	}
	ts.holderName, ts.holderPerson, ts.freeAt = "", "", time.Time{}
	ts.tellKeyboard()
	return true
}

// mine reports whether the keyboard is in its grace and belongs to the
// principal behind this connection, which is what a reconnect is — or, on a
// held session, to its person, which is the same person on another device.
// Called with mu held.
func (ts *termSession) mine(tc *termConn) bool {
	if ts.holder != nil || ts.holderName == "" || !ts.s.now().Before(ts.freeAt) {
		return false
	}
	return ts.holderName == tc.name || (tc.person != "" && tc.person == ts.holderPerson)
}

// follows reports whether the keyboard is this connection's person's, held
// on another of their connections or in its grace, so that typing here
// moves it here without asking anybody. Only a held session's connections
// have a person, and a connection attached to watch or by a watcher never
// types. Called with mu held.
func (ts *termSession) follows(tc *termConn) bool {
	if tc.person == "" || tc.person != ts.holderPerson || ts.holder == tc ||
		tc.watch || ts.lookOnly || !tc.role.allows(RoleControl) {
		return false
	}
	return ts.holder != nil || ts.s.now().Before(ts.freeAt)
}

// move is the keyboard following its person to the connection they typed
// on. Whoever was asking for it is asked for again where it is now. Called
// with mu held.
func (ts *termSession) move(tc *termConn) {
	from := ts.heldBy()
	ts.take(tc)
	for _, cl := range ts.claims {
		ts.askHolder(cl.conn)
	}
	ts.audit("keyboard moved", "from", from, "to", tc.name)
}

// letSendIn lets a send by person in under the keyboard's rule, and holds
// the session for them, in one step: the keyboard free, or held by the same
// person on a connection or in its grace. A person of "" is nobody's, and is
// let in only while the keyboard is free. Otherwise the answer is
// sentKeyboard, naming who holds it, or the session's own when it cannot
// take a send; either way nothing is held.
//
// The keyboard is looked at and the send held under the one lock, mu, so
// that nobody can take a free keyboard between the two and type the
// half-line the send's Enter would submit.
func (ts *termSession) letSendIn(h *heldTerm, person, name string) (*sending, holdwire.SentMsg) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	switch {
	case ts.ended:
		return nil, holdwire.SentMsg{Code: holdwire.SentEnded}
	case !ts.free() && (person == "" || person != ts.holderPerson):
		return nil, holdwire.SentMsg{Code: sentKeyboard, Message: ts.heldBy()}
	}
	return h.hold(person, name)
}

// take gives the keyboard to one connection and tells everyone. Called with
// mu held.
func (ts *termSession) take(tc *termConn) {
	ts.holder, ts.holderName, ts.holderPerson, ts.freeAt = tc, tc.name, tc.person, time.Time{}
	ts.forget(tc)
	ts.tellKeyboard()
}

// letGo frees the keyboard outright. Called with mu held.
func (ts *termSession) letGo() {
	ts.holder, ts.holderName, ts.holderPerson, ts.freeAt = nil, "", "", time.Time{}
	ts.tellKeyboard()
}

// heldBy is the name to show, or "" for nobody. Called with mu held, and
// deliberately without letting a lapsed grace go: describing a terminal must
// not change it.
//
// A terminal that has ended is held by nobody. There is no keyboard left to
// hold — every claim is refused from here on — and a listing that went on
// naming the last person to type would read as though they still were.
func (ts *termSession) heldBy() string {
	if ts.ended {
		return ""
	}
	if ts.holder != nil {
		return ts.holder.name
	}
	if ts.holderName != "" && ts.s.now().Before(ts.freeAt) {
		return ts.holderName
	}
	return ""
}

// claim is somebody asking for the keyboard, and force is the second ask
// after nobody answered the first.
func (ts *termSession) claim(tc *termConn, force bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	switch {
	case ts.ended:
		tc.fail(errTermEnded.Error())
	case ts.lookOnly:
		tc.fail(sharedWatch)
	case tc.watch:
		tc.fail(watchConn)
	case ts.holder == tc:
		tc.fail("you already hold the keyboard")
	case ts.free() || ts.mine(tc):
		ts.take(tc)
		ts.audit("keyboard taken", "to", tc.name)
	case ts.follows(tc):
		ts.move(tc)
	case force:
		ts.forceTo(tc)
	default:
		ts.ask(tc)
	}
}

// ask records the request and puts it in front of the holder. A repeat does
// not restart the clock: the ten seconds are how long this person has been
// waiting, not how long since they last said so.
func (ts *termSession) ask(tc *termConn) {
	for _, cl := range ts.claims {
		if cl.conn == tc {
			ts.askHolder(tc)
			return
		}
	}
	ts.claims = append(ts.claims, &termClaim{conn: tc, at: ts.s.now()})
	ts.askHolder(tc)
}

func (ts *termSession) askHolder(tc *termConn) {
	if ts.holder == nil {
		// Nobody is there to answer — the keyboard is in its grace. The
		// request stands, and the keyboard frame that goes out when the
		// grace lapses is what tells this connection to ask again.
		return
	}
	ts.holder.say(map[string]any{"type": "claim_request", "by": tc.name, "id": tc.id})
}

// forceTo takes the keyboard from a holder who never answered. Called with
// mu held.
func (ts *termSession) forceTo(tc *termConn) {
	for _, cl := range ts.claims {
		if cl.conn != tc {
			continue
		}
		if left := claimWait - ts.s.now().Sub(cl.at); left > 0 {
			tc.fail("nobody has answered yet; force takes the keyboard in " + short(left))
			return
		}
		from := ts.heldBy()
		ts.take(tc)
		ts.audit("keyboard forced", "from", from, "to", tc.name)
		return
	}
	tc.fail("ask for the keyboard first: force only takes one that has gone unanswered for " + short(claimWait))
}

// grant is the holder handing the keyboard to whoever asked for it.
func (ts *termSession) grant(tc *termConn, to string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.holds(tc) {
		tc.fail(notHolding)
		return
	}
	for _, cl := range ts.claims {
		if cl.conn.id != to {
			continue
		}
		if !ts.attached(cl.conn) {
			break
		}
		from := tc.name
		ts.take(cl.conn)
		ts.audit("keyboard granted", "from", from, "to", cl.conn.name)
		return
	}
	tc.fail("nobody with that id is asking for the keyboard")
}

// deny is the holder saying no, which is an answer and not silence: the
// asker is told, and their request is dropped, so forcing it means asking
// again first.
func (ts *termSession) deny(tc *termConn, to string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.holds(tc) {
		tc.fail(notHolding)
		return
	}
	for _, cl := range ts.claims {
		if cl.conn.id != to {
			continue
		}
		cl.conn.fail(tc.name + " kept the keyboard")
		ts.forget(cl.conn)
		return
	}
	tc.fail("nobody with that id is asking for the keyboard")
}

// giveUp is the holder letting go.
func (ts *termSession) giveUp(tc *termConn) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if !ts.holds(tc) {
		tc.fail(notHolding)
		return
	}
	ts.letGo()
	ts.audit("keyboard released", "from", tc.name)
}

// forget drops one connection's outstanding request. Called with mu held.
func (ts *termSession) forget(tc *termConn) {
	out := ts.claims[:0]
	for _, cl := range ts.claims {
		if cl.conn != tc {
			out = append(out, cl)
		}
	}
	ts.claims = out
}

// lapse is the grace running out: the keyboard is free, and everyone is
// told. The timer is a nudge rather than the rule — free is what decides,
// and it is consulted by every path that cares.
func (ts *termSession) lapse() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.free()
}

/* --------------------------------------------------- attaching and leaving --- */

// attach adds one connection, replays what it asked for, and hands it the
// keyboard when it may have it and nobody else does.
//
// A held terminal's viewer that said nothing is shown the holder's screen
// instead of the scrollback, then the bytes after the offset that screen
// stands for. The screen is asked for before the lock is taken: the answer
// can take holdScreenWait, and the output pump needs the lock meanwhile.
func (ts *termSession) attach(tc *termConn, since int64, replayAll bool) {
	var scr *heldScreen
	if h, ok := ts.back.(*heldTerm); ok && replayAll {
		scr = h.screen()
	}
	ts.mu.Lock()
	ts.idleAt = time.Time{}
	// The keyboard is taken before this connection joins the others, so
	// they are told as they always were, and this one only after its hello,
	// which is the first frame, always.
	took := false
	if !tc.watch && !ts.lookOnly && (ts.free() || ts.mine(tc)) {
		ts.take(tc)
		took = true
		ts.audit("keyboard taken", "to", tc.name)
	}
	ts.conns = append(ts.conns, tc)
	from := since
	if replayAll {
		from = ts.ring.Oldest()
	}
	if scr != nil && scr.head.Offset < ts.ring.Oldest() {
		scr = nil // the ring has moved past it: what lay between is gone
	}
	if scr != nil {
		from = scr.head.Offset
	}
	data, at := ts.ring.Read(from)
	gap := at > from
	if scr != nil {
		// A screen can stand for bytes this server has yet to be sent.
		// Until they come, the offset is the screen's and not the ring's,
		// and this viewer skips them when they do.
		at, gap = scr.head.Offset, false
		if at > ts.ring.End() {
			tc.skip = at
		}
	}
	hello := map[string]any{
		"type": "hello", "id": ts.id, "kind": ts.kind,
		"cols": ts.cols, "rows": ts.rows, "offset": at,
		"holder":  nameOrNil(ts.heldBy()),
		"you":     map[string]any{"name": tc.name, "role": tc.role, "conn": tc.id},
		"viewers": ts.viewers(),
	}
	if ts.kind == termKindShared {
		// In the first frame, and not only in the description, because the
		// page has to know before it draws anything that this terminal's
		// size is not its to set and may not be its to type into.
		hello["mode"] = ts.shareMode()
	}
	if scr != nil {
		hello["repaint"] = true
	}
	ts.holderConn(hello)
	tc.say(hello)
	if took {
		tc.say(ts.keyboardDoc())
	}
	if gap {
		tc.say(map[string]any{"type": "gap", "from": from, "to": at})
	}
	if scr != nil {
		// Never empty: heldTerm.screen gives no screen rather than that.
		tc.queue(termFrame{op: opBinary, data: scr.repaint, alone: true})
	}
	if len(data) > 0 {
		tc.pushOutput(data)
	}
	if ts.ended {
		tc.say(map[string]any{"type": "exit", "code": ts.exit})
	}
	ts.tellViewers()
	ts.mu.Unlock()
	ts.audit("terminal attached", "name", tc.name, "role", tc.role)
}

// detach is one connection going away. The terminal keeps running — that is
// what it is for — and the keyboard stays with whoever dropped, for the
// grace, so a reconnect gets it back rather than finding somebody typing.
func (ts *termSession) detach(tc *termConn) {
	ts.mu.Lock()
	out := ts.conns[:0]
	for _, c := range ts.conns {
		if c != tc {
			out = append(out, c)
		}
	}
	ts.conns = out
	ts.forget(tc)
	if ts.holder == tc {
		ts.holder = nil
		ts.holderName, ts.freeAt = tc.name, ts.s.now().Add(keyboardGrace)
		time.AfterFunc(keyboardGrace, ts.lapse)
	}
	if len(ts.conns) == 0 {
		ts.idleAt = ts.s.now()
	}
	ts.tellViewers()
	ts.mu.Unlock()
	ts.audit("terminal detached", "name", tc.name, "role", tc.role)
}

// attached reports whether this connection is still one of ours. Called with
// mu held.
func (ts *termSession) attached(tc *termConn) bool {
	for _, c := range ts.conns {
		if c == tc {
			return true
		}
	}
	return false
}

/* -------------------------------------------------------- what goes out --- */

// tell sends one document to everyone attached. Called with mu held.
func (ts *termSession) tell(doc map[string]any) {
	for _, tc := range ts.conns {
		tc.say(doc)
	}
}

// tellGap is tell for a gap in the output, which a held session's viewer
// that is behind is not sent (termFrame.covered). Called with mu held.
func (ts *termSession) tellGap(doc map[string]any) {
	raw, err := rota.Encode(doc)
	if err != nil {
		return
	}
	for _, tc := range ts.conns {
		tc.queue(termFrame{op: opText, data: raw, covered: true})
	}
}

func (ts *termSession) tellKeyboard() { ts.tell(ts.keyboardDoc()) }

// keyboardDoc is the keyboard frame: who holds it now. Called with mu held.
func (ts *termSession) keyboardDoc() map[string]any {
	doc := map[string]any{"type": "keyboard", "holder": nameOrNil(ts.heldBy())}
	ts.holderConn(doc)
	return doc
}

// holderConn adds, on a held session, which connection holds the keyboard:
// one person may be attached from several devices, and only this tells
// them which of them it is. null is none — nobody, or the grace. Called with
// mu held.
func (ts *termSession) holderConn(doc map[string]any) {
	if !ts.held() {
		return
	}
	doc["holder_conn"] = nil
	if ts.holder != nil && !ts.ended {
		doc["holder_conn"] = ts.holder.id
	}
}

func (ts *termSession) tellViewers() {
	ts.tell(map[string]any{"type": "viewers", "list": ts.viewers()})
}

// viewers is who is watching: a name, a role and how long they have been
// there, and never an address — a watcher's location is nobody else's
// business. Called with mu held.
func (ts *termSession) viewers() []map[string]any {
	out := make([]map[string]any, 0, len(ts.conns))
	for _, tc := range ts.conns {
		out = append(out, map[string]any{
			"name": tc.name, "role": tc.role, "since": tc.since.Format(time.RFC3339),
		})
	}
	return out
}

// audit writes one line about this terminal. Every line names the terminal
// and nothing that was typed or printed: a log is kept for far longer than a
// session, by people who were never meant to read over somebody's shoulder.
func (ts *termSession) audit(msg string, args ...any) {
	ts.log(msg, append([]any{"terminal", ts.id}, args...)...)
}

/* -------------------------------------------------------- the output pump --- */

// output keeps what was printed and fans it out. One copy is made and shared:
// the frames are never written to again, and copying per connection would
// multiply every burst by the number of people watching.
func (ts *termSession) output(p []byte) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.ring.Write(chunk)
	if ts.rec != nil {
		ts.rec.write(chunk)
	}
	for _, tc := range ts.conns {
		tc.push(opBinary, chunk)
	}
}

// outputAt is output from a holder, which says where in the stream it
// belongs. Bytes this end already has are dropped. Bytes that never reached
// it are said as a gap, and the ring starts again at the holder's numbering,
// so the next link asks for exactly what it lacks. A viewer whose screen
// already shows some of these skips them.
func (ts *termSession) outputAt(off int64, p []byte) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	end := ts.ring.End()
	if off < end {
		if int64(len(p)) <= end-off {
			return
		}
		p, off = p[end-off:], end
	}
	if off > end {
		ts.tellGap(map[string]any{"type": "gap", "from": end, "to": off, "bytes": off - end})
		ts.ring = ring.NewAt(ts.s.opts.Terminal.Scrollback, off)
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	ts.ring.Write(chunk)
	if ts.rec != nil {
		ts.rec.writeAt(off, chunk)
	}
	for _, tc := range ts.conns {
		switch skip := tc.skip - off; {
		case skip <= 0:
			tc.push(opBinary, chunk)
		case skip < int64(len(chunk)):
			tc.push(opBinary, chunk[skip:])
		default:
			continue
		}
		tc.skip = 0
	}
}

// catchUp is what a held session's viewer that fell behind is sent instead
// of being closed: a repaint frame saying where its offsets go on from and
// at what size, the holder's screen as it is now, and the output after the
// offset that screen stands for. It is called on the viewer's writing
// goroutine with its queue drained, and asks for the screen before taking
// the lock, as attach does; the output that arrives meanwhile is on the
// screen or after it. It asks twice before it does without: a holder
// flooded with output is slowest to answer, and a flood is when a viewer
// falls behind.
//
// Without a screen the viewer is reset and replayed the last termTail of
// the ring, after a gap for what is kept before that: the whole scrollback
// is more than a viewer that just fell behind takes in before it falls
// behind again. The output goes in frames of at most termJoin, each written
// with its own deadline.
func (ts *termSession) catchUp(tc *termConn) []termFrame {
	var scr *heldScreen
	if h, ok := ts.back.(*heldTerm); ok {
		if scr = h.screen(); scr == nil {
			scr = h.screen()
		}
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	oldest := ts.ring.Oldest()
	screened := scr != nil && scr.head.Offset >= oldest
	paint, from := []byte(termReset), max(oldest, ts.ring.End()-termTail)
	if screened {
		paint, from = scr.repaint, scr.head.Offset
	}
	tail, _ := ts.ring.Read(from)
	tc.skip = 0
	if from > ts.ring.End() {
		tc.skip = from // a screen ahead of what this server has been sent
	}
	tc.behind.Store(false)
	doc, _ := rota.Encode(map[string]any{"type": "repaint", "offset": from, "cols": ts.cols, "rows": ts.rows})
	out := []termFrame{{op: opText, data: doc}, {op: opBinary, data: paint}}
	if !screened && from > oldest {
		gap, _ := rota.Encode(map[string]any{"type": "gap", "from": oldest, "to": from, "bytes": from - oldest})
		out = append(out, termFrame{op: opText, data: gap})
	}
	for _, piece := range pieces(tail) {
		out = append(out, termFrame{op: opBinary, data: piece})
	}
	return out
}

// end is the offset this terminal's output has got to.
func (ts *termSession) end() int64 {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.ring.End()
}

/* ------------------------------------------------------------- the ending --- */

// over reports whether this terminal has ended, which is the one thing every
// endpoint has to know before it tries anything.
func (ts *termSession) over() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.ended
}

// run is the whole of a terminal after it has started, and the one place it
// ends: the far end prints until there is nothing more and then says how it
// ended, and everything that follows from the ending — the exit frame, the
// recording, the account, the sockets — happens after both, once, here.
//
// A held terminal this server let go of returns from its far end without an
// ending: it has not ended, it has stopped being this server's.
func (ts *termSession) run() {
	code := ts.back.run(ts)
	ts.mu.Lock()
	left := ts.left
	ts.mu.Unlock()
	if left {
		if ts.rec != nil {
			ts.rec.close(ts.view())
		}
		return
	}
	ts.finish(code)
}

// held reports whether this terminal's CLI is in a holder of its own.
func (ts *termSession) held() bool {
	_, ok := ts.back.(*heldTerm)
	return ok
}

// leave lets go of a held terminal, which goes on running for the next
// daemon on the store to take up.
func (ts *termSession) leave() {
	ts.mu.Lock()
	ts.left = true
	ts.mu.Unlock()
	ts.back.detach()
}

// finish is the process having gone: everyone is told how, the account is
// let go of, and the terminal stays listed for a while so a client that
// reattaches a moment too late reads the exit rather than a 404 it cannot
// tell from a wrong id.
//
// The exit frame is queued to every connection before anything is closed,
// and each connection is then settled rather than simply stopped — it has to
// have written that frame before its socket is allowed to say goodbye, or a
// client learns that the terminal is over without ever learning how.
func (ts *termSession) finish(code int) {
	ts.mu.Lock()
	if ts.ended {
		ts.mu.Unlock()
		return
	}
	ts.ended, ts.exit = true, code
	ts.tell(map[string]any{"type": "exit", "code": code})
	conns := append([]*termConn(nil), ts.conns...)
	// Read before anybody can see this terminal has ended, so whoever
	// waited for that and then changed keepEnded changed it after.
	keep := keepEnded
	ts.mu.Unlock()

	ts.audit("terminal ended", "exit", code)
	if ts.kind == termKindShared {
		// Beside it rather than instead of it: a shared terminal ends like
		// any other, and this is the other half of the line that said it
		// had been offered.
		ts.audit("terminal unshared", "exit", code)
	}
	// The pump has returned, so everything this terminal printed is in the
	// ring and in the recording; both are finished with here.
	if ts.rec != nil {
		ts.rec.close(ts.view())
	}
	if ts.release != nil {
		ts.release()
	}
	ts.back.close()
	for _, tc := range conns {
		tc.settle()
	}
	time.AfterFunc(keep, func() { ts.s.dropTerminal(ts.id) })
}

// kill asks the terminal to end and makes sure it does: a hangup first,
// which is what closing a terminal window sends and what a CLI is written to
// tidy up after, and then the ending nothing survives.
//
// For a shared terminal the hangup is a frame rather than a signal — this
// server has no process to signal — and the sharer hangs up the child it is
// holding. The insisting ending is this server letting go of the link, which
// is all it can do to a machine it does not own.
func (ts *termSession) kill(by string) {
	ts.mu.Lock()
	if ts.ended || ts.killing || ts.left {
		ts.mu.Unlock()
		return
	}
	ts.killing = true
	back := ts.back
	ts.mu.Unlock()
	if by != "" {
		ts.audit("terminal killed", "by", by)
	}
	back.hangUp()
	wait := termKillAfter
	if ts.held() {
		// A holder kills its own CLI at termKillAfter, and records it: it
		// is given the time to, so the session reads ended, not interrupted.
		wait += holdKillLater
	}
	time.AfterFunc(wait, func() {
		if !ts.over() {
			back.end()
		}
	})
}

/* ------------------------------------------------------- what it looks like --- */

func (ts *termSession) view() map[string]any {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.describe()
}

// describe is the document the page's information panel is built from: what
// is running, who is looking, where the output has got to, and when the
// login inside will lapse. Called with mu held.
func (ts *termSession) describe() map[string]any {
	doc := map[string]any{
		"id": ts.id, "kind": ts.kind, "label": ts.name, "cwd": ts.cwd,
		"started": ts.started.Format(time.RFC3339),
		"cols":    ts.cols, "rows": ts.rows,
		"holder":  nameOrNil(ts.heldBy()),
		"viewers": ts.viewers(),
		"offset":  ts.ring.End(),
		"ended":   ts.ended,
		// Whether what scrolls past is being kept, which is not something a
		// person should have to read a configuration file to find out.
		"recording": ts.rec != nil,
	}
	if ts.kind != termKindShell && ts.account != 0 {
		doc["account"] = map[string]any{"id": ts.account, "label": ts.label, "provider": ts.provider}
	}
	// What only this terminal's far end knows: for a shared one, the process
	// answering for it, whether it may be typed into, and whether the rota
	// holding it is still there.
	ts.back.describe(doc)
	if ts.ended {
		doc["exit"] = ts.exit
	}
	if !ts.tokenUntil.IsZero() {
		doc["token_until"] = ts.tokenUntil.Format(time.RFC3339)
	}
	return doc
}

/* ---------------------------------------------------------- the recording --- */

// recorder keeps a terminal's output on disk, and only its output: what was
// typed is never written down. A recording is a transcript of somebody's
// working session, so the file is theirs alone to read and it stops at a
// size rather than filling the disk.
//
// A held session's output says where in the whole of it each piece
// belongs, and the next daemon takes its recording up and adds to it, so
// the recording keeps where in the output it has got to: end. Where what
// it is given does not follow on from there — a gap the link could not
// fill, or a holder that kept less than a daemon missed — it says so in
// <id>.gaps before it writes on, one line "<bytes in the file> <offset in
// the output>" for each place the file skips to. With no such file the
// recording started at the start and never skipped. Nothing is recorded
// twice, and nothing is skipped without saying so.
type recorder struct {
	out  *os.File
	meta string
	gaps string
	max  int64
	n    int64
	full bool
	end  int64
}

// newRecorder starts a recording, or with resume takes up the one already
// there for this id — a held session a daemon before this one recorded —
// and adds to it, from where in the output it had got to.
func newRecorder(dir, id string, max int64, resume bool) (*recorder, error) {
	r := &recorder{meta: filepath.Join(dir, id+".json"), gaps: filepath.Join(dir, id+".gaps"), max: max}
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if !resume {
		// A recording started afresh owes nothing to where one before it
		// skipped.
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		if err := os.Remove(r.gaps); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, id+".out"), flags, 0o600)
	if err != nil {
		return nil, err
	}
	r.out = f
	if resume {
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, err
		}
		r.n, r.full, r.end = fi.Size(), fi.Size() >= max, fi.Size()
		if at, from, ok := lastGap(r.gaps); ok && at <= r.n {
			r.end = from + r.n - at
		}
	}
	return r, nil
}

// lastGap is the last place a recording's <id>.gaps says its file skips
// to. A line that is not whole — the one being written when a daemon went —
// is passed over: nothing was written after it.
func lastGap(path string) (at, from int64, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	for line := range strings.Lines(string(raw)) {
		f := strings.Fields(line)
		if !strings.HasSuffix(line, "\n") || len(f) != 2 {
			continue
		}
		a, aerr := strconv.ParseInt(f[0], 10, 64)
		b, berr := strconv.ParseInt(f[1], 10, 64)
		if aerr == nil && berr == nil && a >= 0 && b >= 0 {
			at, from, ok = a, b, true
		}
	}
	return at, from, ok
}

// writeAt is write for output that says where in the whole of it it
// belongs: what the recording has already is left out, and a place it
// skips to is written down before what follows it is.
func (r *recorder) writeAt(off int64, p []byte) {
	if r.full || r.out == nil {
		return
	}
	if skip := r.end - off; skip > 0 {
		if skip >= int64(len(p)) {
			return
		}
		p, off = p[skip:], r.end
	}
	if off > r.end && !r.skipTo(off) {
		// A recording that could not say where it skipped to would be
		// wrong about everything after: it stops here instead.
		r.full = true
		return
	}
	n := r.n
	r.write(p)
	r.end = off + r.n - n
}

// skipTo writes down that the file goes on, from the byte it has reached,
// with the output at off.
func (r *recorder) skipTo(off int64) bool {
	f, err := os.OpenFile(r.gaps, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return false
	}
	_, err = fmt.Fprintf(f, "%d %d\n", r.n, off)
	return errors.Join(err, f.Close()) == nil
}

// write appends output up to the cap and then stops. Called with the
// session's mu held, so the file is written from one goroutine at a time.
func (r *recorder) write(p []byte) {
	if r.full || r.out == nil {
		return
	}
	if r.n+int64(len(p)) > r.max {
		p, r.full = p[:r.max-r.n], true
	}
	if len(p) > 0 {
		n, err := r.out.Write(p)
		r.n += int64(n)
		if err != nil {
			r.full = true
		}
	}
}

// close writes what the terminal was, beside what it printed, so a recording
// found later says whose it was and when.
func (r *recorder) close(doc map[string]any) {
	if r.out == nil {
		return
	}
	r.out.Close()
	r.out = nil
	if raw, err := rota.Encode(doc); err == nil {
		_ = os.WriteFile(r.meta, append(raw, '\n'), 0o600)
	}
}

/* -------------------------------------------------------------- the registry --- */

func (s *Server) addTerminal(ts *termSession) {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	s.terms[ts.id] = ts
}

func (s *Server) findTerminal(id string) *termSession {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	return s.terms[id]
}

func (s *Server) dropTerminal(id string) {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	delete(s.terms, id)
}

func (s *Server) everyTerminal() []*termSession {
	s.termsMu.Lock()
	defer s.termsMu.Unlock()
	out := make([]*termSession, 0, len(s.terms))
	for _, ts := range s.terms {
		out = append(out, ts)
	}
	return out
}

// live counts the terminals still running, which is what max_sessions caps:
// one that has ended is listed for a few minutes but is not holding
// anything. One still starting holds its place already. A daemon also
// counts every holder running on its store that it is not linked to: each
// is a CLI holding an account. The records are read first and the rest in
// one look after, so a terminal moving from one to another in between is
// counted once — a held one still starting is counted by its place, not by
// its record.
func (s *Server) liveTerminals() int {
	var records []holdwire.Record
	if s.terminals != "" {
		records = holdwire.Scan(s.terminals)
	}
	s.termsMu.Lock()
	n := s.starting
	all := make([]*termSession, 0, len(s.terms))
	for _, ts := range s.terms {
		all = append(all, ts)
	}
	for _, r := range records {
		if r.Running() && s.terms[r.ID] == nil && !s.pending[r.ID] {
			n++
		}
	}
	s.termsMu.Unlock()
	for _, ts := range all {
		if !ts.over() {
			n++
		}
	}
	return n
}

// sweepTerminals ends the terminals nobody has been attached to for longer
// than the idle timeout. A terminal is meant to outlive a browser tab, not
// to outlive the person: a CLI left running for a week is an account being
// held and a machine being used for nothing.
func (s *Server) sweepTerminals() {
	limit := s.opts.Terminal.IdleTimeout
	if limit <= 0 {
		return
	}
	now := s.now()
	for _, ts := range s.everyTerminal() {
		// A held session is exempt: a long task left alone is what it is
		// for, and it keeps running whether or not anybody watches.
		ts.mu.Lock()
		idle := !ts.held() && !ts.ended && len(ts.conns) == 0 && !ts.idleAt.IsZero() &&
			now.Sub(ts.idleAt) >= limit
		ts.mu.Unlock()
		if idle {
			ts.audit("terminal idle", "for", short(limit))
			ts.kill("idle")
		}
	}
}

// keepTerminals is the sweeper. It runs until the server stops, and it is
// the only thing measuring the idle timeout — a timer per terminal would be
// one more thing to cancel in every path that ends one. A daemon's sweep
// also goes through its store's records (sweepRecords).
func (s *Server) keepTerminals() {
	t := time.NewTicker(termSweep)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.sweepTerminals()
			s.sweepRecords()
		}
	}
}

// endTerminals ends every terminal, which is what stopping the server means
// for them: a CLI on a pseudo-terminal is in a process group of its own
// precisely so a signal to rota does not reach it, so without this, exiting
// would leave agents running.
//
// A held terminal is let go of instead, and is no longer listed here: its
// holder outlives this server by design, and the next daemon takes it up.
func (s *Server) endTerminals() {
	for _, ts := range s.everyTerminal() {
		if ts.held() {
			ts.leave()
			s.dropTerminal(ts.id)
			continue
		}
		ts.kill("shutdown")
	}
}

/* ------------------------------------------------------------------ odds --- */

// nameOrNil is a holder's name, or JSON null for nobody: a page reading
// "holder" should not have to tell an empty name from no name.
func nameOrNil(name string) any {
	if name == "" {
		return nil
	}
	return name
}

// boundSize keeps a terminal to a size a terminal can be, the same bound a
// holder keeps its pty to.
func boundSize(cols, rows uint16) (uint16, uint16) { return holdwire.BoundSize(cols, rows) }

const (
	// defaultScrollback is two megabytes: enough that a person scrolling
	// back finds what they were looking at, small enough that eight
	// terminals are sixteen megabytes.
	defaultScrollback = ring.DefaultKeep
)

// short is a duration in the words a person types, rounded to the second so
// that "9.9999s to wait" reads as the nine seconds it is.
func short(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return d.Round(time.Second).String()
}

// termGone is the answer for a terminal that is over: the id is right, the
// terminal is not there to take anything.
func termGone(w http.ResponseWriter, ts *termSession) {
	fail(w, http.StatusConflict, "terminal "+ts.id+" has ended")
}
