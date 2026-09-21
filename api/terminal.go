package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	rota "github.com/professor93/rota/lib"

	"github.com/professor93/rota/internal/pty"
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
	// termKindAccount is a terminal running an account's vendor CLI, and
	// termKindShell one running the person's login shell — which is off
	// unless the file asks for it.
	termKindAccount = "account"
	termKindShell   = "shell"

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
	// termQueue is how many frames one connection may fall behind by. A
	// terminal fans out to everyone attached, and a reader that has stopped
	// reading must not be able to hold up the terminal or anybody else: past
	// this it is closed and told to come back with an offset. A variable, so
	// a test about falling behind need not print a megabyte first.
	termQueue = 256
)

// The two refusals a connection is given for trying to type. They are
// constants because they are part of what this server answers, and a test
// reads them.
const (
	notHolding = "you are not holding the keyboard"
	watchConn  = "this connection attached to watch; attach again with mode=control to ask for the keyboard"
)

var (
	errNotHolding = errors.New(notHolding)
	errTermEnded  = errors.New("this terminal has ended")
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

/* ------------------------------------------------------- what was printed --- */

// outRing is the last bytes a terminal printed, with absolute offsets: a
// client says where it got to as a number that never repeats, and the ring
// answers with everything since — or says how much it no longer has.
//
// It is circular because a terminal prints for hours. A slice that dropped
// its head would copy the whole scrollback on every burst of output.
type outRing struct {
	buf   []byte
	size  int   // bytes held
	start int   // index of the oldest byte
	end   int64 // absolute offset just past the newest byte
}

func newOutRing(keep int) *outRing {
	if keep <= 0 {
		keep = defaultScrollback
	}
	return &outRing{buf: make([]byte, keep)}
}

func (r *outRing) write(p []byte) {
	r.end += int64(len(p))
	if len(p) >= len(r.buf) {
		// A burst larger than the whole ring: only its tail can be kept.
		copy(r.buf, p[len(p)-len(r.buf):])
		r.start, r.size = 0, len(r.buf)
		return
	}
	at := (r.start + r.size) % len(r.buf)
	n := copy(r.buf[at:], p)
	copy(r.buf, p[n:])
	if r.size += len(p); r.size > len(r.buf) {
		r.start = (r.start + r.size - len(r.buf)) % len(r.buf)
		r.size = len(r.buf)
	}
}

// oldest is the lowest offset this ring can still answer for.
func (r *outRing) oldest() int64 { return r.end - int64(r.size) }

// read is everything from since onwards, and the offset the answer actually
// starts at — which is later than asked for when the ring has moved on. The
// caller compares the two to know whether to say so first.
func (r *outRing) read(since int64) ([]byte, int64) {
	from := since
	if oldest := r.oldest(); from < oldest {
		from = oldest
	}
	if from > r.end {
		from = r.end
	}
	skip := int(from - r.oldest())
	n := r.size - skip
	if n <= 0 {
		return nil, from
	}
	out := make([]byte, 0, n)
	i := (r.start + skip) % len(r.buf)
	if i+n <= len(r.buf) {
		out = append(out, r.buf[i:i+n]...)
	} else {
		out = append(out, r.buf[i:]...)
		out = append(out, r.buf[:n-(len(r.buf)-i)]...)
	}
	return out, from
}

/* ----------------------------------------------------------- a connection --- */

// termFrame is one thing the server sends a connection: output as bytes, or
// a document as text. Both go through one queue so they keep their order —
// a keyboard change that overtook the output it explains would read as a
// lie.
type termFrame struct {
	op   byte
	data []byte
}

// termConn is one attached socket: who is behind it, whether it may ever
// type, and the bounded queue everything it is sent goes through.
type termConn struct {
	id    string
	name  string
	role  Role
	watch bool // this connection may never hold the keyboard
	since time.Time

	c    *wsConn
	out  chan termFrame
	gone chan struct{}
	once sync.Once
	// pending is a document that turned up while output was being joined
	// together, kept to be sent next so nothing overtakes anything. It
	// belongs to the writing goroutine alone.
	pending *termFrame
	// slow says this connection was closed for falling behind, so the close
	// frame can say which of the two it was.
	slow atomic.Bool
}

func newTermConn(c *wsConn, p *Principal, watch bool, at time.Time) *termConn {
	return &termConn{
		id: runID(), name: p.Name, role: p.Role, watch: watch, since: at,
		c: c, out: make(chan termFrame, termQueue), gone: make(chan struct{}),
	}
}

// push queues one frame, and closes the connection rather than waiting when
// the queue is full. That is the whole rule that keeps one slow watcher from
// stalling the terminal: the terminal never blocks on a reader.
func (tc *termConn) push(op byte, data []byte) {
	select {
	case tc.out <- termFrame{op: op, data: data}:
	default:
		tc.slow.Store(true)
		tc.stop()
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

// write is this connection's one writing goroutine: everything the terminal
// says to it, in order, on the socket. When the connection is finished it
// drains what is already queued first, so the exit frame that was pushed
// just before the close still arrives.
func (tc *termConn) write() {
	for {
		var f termFrame
		switch {
		case tc.pending != nil:
			f, tc.pending = *tc.pending, nil
		default:
			select {
			case f = <-tc.out:
			case <-tc.gone:
				for {
					select {
					case f := <-tc.out:
						if err := tc.c.send(f.op, f.data); err != nil {
							return
						}
					default:
						return
					}
				}
			}
		}
		if f.op == opBinary {
			f.data = tc.join(f.data)
		}
		if err := tc.c.send(f.op, f.data); err != nil {
			tc.stop()
			return
		}
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
			if f.op != opBinary {
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
	// tokenUntil is when the login inside this terminal lapses: the
	// long-lived token's date where the account has one, the access token's
	// expiry otherwise. A person looking at a terminal that has been open
	// for a day wants to know that before the CLI tells them.
	tokenUntil time.Time

	master  *os.File
	proc    *os.Process
	release func()
	rec     *recorder

	mu         sync.Mutex
	cols, rows uint16
	ring       *outRing
	conns      []*termConn
	holder     *termConn
	// holderName and freeAt are the keyboard's grace: whose it still is
	// after their socket dropped, and until when.
	holderName string
	freeAt     time.Time
	claims     []*termClaim
	ended      bool
	exit       int
	// idleAt is when the last connection left, and zero while somebody is
	// attached. The idle timeout is measured from it.
	idleAt  time.Time
	killing bool
}

/* ------------------------------------------------------------ the keyboard --- */

// holds is the keyboard rule itself, and the only statement of it: this
// exact connection is the holder, it is one that may ever hold the keyboard,
// and the principal behind it may control. Identity and role, not the page's
// good manners.
//
// Called with mu held.
func (ts *termSession) holds(tc *termConn) bool {
	return tc != nil && ts.holder == tc && !tc.watch && tc.role.allows(RoleControl)
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
	if !ts.holds(tc) {
		ts.mu.Unlock()
		return errNotHolding
	}
	master := ts.master
	ts.mu.Unlock()
	_, err := master.Write(p)
	return err
}

// resizeTo is the other thing only the holder may do: the size is the
// terminal's, not the viewer's, and a watcher whose window is narrower must
// not reflow everybody else's screen.
func (ts *termSession) resizeTo(tc *termConn, cols, rows uint16) error {
	cols, rows = boundSize(cols, rows)
	ts.mu.Lock()
	if ts.ended {
		ts.mu.Unlock()
		return errTermEnded
	}
	if !ts.holds(tc) {
		ts.mu.Unlock()
		return errNotHolding
	}
	master := ts.master
	ts.mu.Unlock()
	if err := pty.Resize(master, cols, rows); err != nil {
		return err
	}
	ts.mu.Lock()
	ts.cols, ts.rows = cols, rows
	ts.tell(map[string]any{"type": "resized", "cols": cols, "rows": rows})
	ts.mu.Unlock()
	return nil
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
	ts.holderName, ts.freeAt = "", time.Time{}
	ts.tellKeyboard()
	return true
}

// mine reports whether the keyboard is in its grace and belongs to the
// principal behind this connection, which is what a reconnect is. Called
// with mu held.
func (ts *termSession) mine(tc *termConn) bool {
	return ts.holder == nil && ts.holderName != "" && ts.holderName == tc.name &&
		ts.s.now().Before(ts.freeAt)
}

// take gives the keyboard to one connection and tells everyone. Called with
// mu held.
func (ts *termSession) take(tc *termConn) {
	ts.holder, ts.holderName, ts.freeAt = tc, tc.name, time.Time{}
	ts.forget(tc)
	ts.tellKeyboard()
}

// letGo frees the keyboard outright. Called with mu held.
func (ts *termSession) letGo() {
	ts.holder, ts.holderName, ts.freeAt = nil, "", time.Time{}
	ts.tellKeyboard()
}

// heldBy is the name to show, or "" for nobody. Called with mu held, and
// deliberately without letting a lapsed grace go: describing a terminal must
// not change it.
func (ts *termSession) heldBy() string {
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
	case tc.watch:
		tc.fail(watchConn)
	case ts.holder == tc:
		tc.fail("you already hold the keyboard")
	case ts.free() || ts.mine(tc):
		ts.take(tc)
		ts.audit("keyboard taken", "to", tc.name)
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
func (ts *termSession) attach(tc *termConn, since int64, replayAll bool) {
	ts.mu.Lock()
	ts.conns = append(ts.conns, tc)
	ts.idleAt = time.Time{}
	if !tc.watch && (ts.free() || ts.mine(tc)) {
		ts.take(tc)
		ts.audit("keyboard taken", "to", tc.name)
	}
	from := since
	if replayAll {
		from = ts.ring.oldest()
	}
	data, at := ts.ring.read(from)
	gap := at > from
	tc.say(map[string]any{
		"type": "hello", "id": ts.id, "kind": ts.kind,
		"cols": ts.cols, "rows": ts.rows, "offset": at,
		"holder":  nameOrNil(ts.heldBy()),
		"you":     map[string]any{"name": tc.name, "role": tc.role, "conn": tc.id},
		"viewers": ts.viewers(),
	})
	if gap {
		tc.say(map[string]any{"type": "gap", "from": from, "to": at})
	}
	if len(data) > 0 {
		tc.push(opBinary, data)
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

func (ts *termSession) tellKeyboard() {
	ts.tell(map[string]any{"type": "keyboard", "holder": nameOrNil(ts.heldBy())})
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

// pump reads the terminal and gives what it read to everyone at once. It is
// the only reader of the master.
func (ts *termSession) pump() {
	buf := make([]byte, termRead)
	for {
		n, err := ts.master.Read(buf)
		if n > 0 {
			ts.output(buf[:n])
		}
		if err != nil {
			// A master whose child has gone reads as an error rather than as
			// an end on Linux. Both mean the same thing here.
			return
		}
	}
}

// output keeps what was printed and fans it out. One copy is made and shared:
// the frames are never written to again, and copying per connection would
// multiply every burst by the number of people watching.
func (ts *termSession) output(p []byte) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.ring.write(chunk)
	if ts.rec != nil {
		ts.rec.write(chunk)
	}
	for _, tc := range ts.conns {
		tc.push(opBinary, chunk)
	}
}

/* ------------------------------------------------------------- the ending --- */

// over reports whether this terminal has ended, which is the one thing every
// endpoint has to know before it tries anything.
func (ts *termSession) over() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.ended
}

// finish is the process having gone: everyone is told how, the account is
// let go of, and the terminal stays listed for a while so a client that
// reattaches a moment too late reads the exit rather than a 404 it cannot
// tell from a wrong id.
func (ts *termSession) finish(code int) {
	ts.mu.Lock()
	if ts.ended {
		ts.mu.Unlock()
		return
	}
	ts.ended, ts.exit = true, code
	ts.tell(map[string]any{"type": "exit", "code": code})
	conns := append([]*termConn(nil), ts.conns...)
	ts.mu.Unlock()

	ts.audit("terminal ended", "exit", code)
	if ts.rec != nil {
		ts.rec.close(ts.view())
	}
	if ts.release != nil {
		ts.release()
	}
	ts.master.Close()
	for _, tc := range conns {
		tc.stop()
	}
	time.AfterFunc(keepEnded, func() { ts.s.dropTerminal(ts.id) })
}

// kill asks the terminal to end and makes sure it does: a hangup first,
// which is what closing a terminal window sends and what a CLI is written to
// tidy up after, and then the signal nothing survives.
func (ts *termSession) kill(by string) {
	ts.mu.Lock()
	if ts.ended || ts.killing {
		ts.mu.Unlock()
		return
	}
	ts.killing = true
	proc := ts.proc
	ts.mu.Unlock()
	if by != "" {
		ts.audit("terminal killed", "by", by)
	}
	if proc == nil {
		return
	}
	_ = proc.Signal(syscall.SIGHUP)
	time.AfterFunc(termKillAfter, func() {
		if !ts.over() {
			_ = proc.Signal(syscall.SIGKILL)
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
		"offset":  ts.ring.end,
		"ended":   ts.ended,
		// Whether what scrolls past is being kept, which is not something a
		// person should have to read a configuration file to find out.
		"recording": ts.rec != nil,
	}
	if ts.kind == termKindAccount {
		doc["account"] = map[string]any{"id": ts.account, "label": ts.label, "provider": ts.provider}
	}
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
type recorder struct {
	out  *os.File
	meta string
	max  int64
	n    int64
	full bool
}

func newRecorder(dir, id string, max int64) (*recorder, error) {
	f, err := os.OpenFile(filepath.Join(dir, id+".out"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &recorder{out: f, meta: filepath.Join(dir, id+".json"), max: max}, nil
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
// anything.
func (s *Server) liveTerminals() int {
	n := 0
	for _, ts := range s.everyTerminal() {
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
		ts.mu.Lock()
		idle := !ts.ended && len(ts.conns) == 0 && !ts.idleAt.IsZero() &&
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
// one more thing to cancel in every path that ends one.
func (s *Server) keepTerminals() {
	t := time.NewTicker(termSweep)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.sweepTerminals()
		}
	}
}

// endTerminals ends every terminal, which is what stopping the server means
// for them: a CLI on a pseudo-terminal is in a process group of its own
// precisely so a signal to rota does not reach it, so without this, exiting
// would leave agents running.
func (s *Server) endTerminals() {
	for _, ts := range s.everyTerminal() {
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

// boundSize keeps a terminal to a size a terminal can be. Zero is the
// default rather than an error — a client that says nothing means the
// ordinary eighty by twenty-four — and the ceiling is there because the
// numbers arrive from whoever is connected.
func boundSize(cols, rows uint16) (uint16, uint16) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	if cols > maxTermCols {
		cols = maxTermCols
	}
	if rows > maxTermRows {
		rows = maxTermRows
	}
	return cols, rows
}

const (
	maxTermCols = 1000
	maxTermRows = 1000
	// defaultScrollback is two megabytes: enough that a person scrolling
	// back finds what they were looking at, small enough that eight
	// terminals are sixteen megabytes.
	defaultScrollback = 2 << 20
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
