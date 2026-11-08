package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/internal/vtscan"
)

// A held terminal is one whose CLI this server does not hold at all.
//
// With Options.HoldCommand set, each terminal is started in a process of its
// own — a holder, `rota serve hold` — which owns the pseudo-terminal, the
// CLI on it, an emulator of its screen and a ring of what it printed, and
// which outlives this server. This server links to it over the socket in the
// session's directory and is, from then on, what it always was for a
// terminal: the ring, the fan-out, the keyboard, the audit. Only the far end
// changed, so heldTerm is one more termBackend beside localTerm and
// sharedTerm.
//
// The difference that matters is the ending. Stopping this server lets go
// of a held terminal instead of ending it, and the next daemon on the store
// links to it again; the holder keeps the account's claim for as long as
// its CLI lives. Only one daemon may serve a store at a time, which is the
// claim New takes on daemon.lock.

var (
	// holdBeat is how often this end pings a holder, and a third of how long
	// a link may say nothing before it is treated as gone: the holder's own
	// Beat, so both ends judge a link alike.
	holdBeat = 10 * time.Second
	// holdHandshake bounds dialling a holder and its first frames.
	holdHandshake = 2 * time.Second
	// holdRedial is how often a lost link to a holder that is still alive
	// is dialled again.
	holdRedial = time.Second
	// holdReady is how long a holder has to say it started.
	holdReady = 30 * time.Second
	// holdScreenWait is how long an attach waits for a holder's screen
	// before it is replayed the ring instead.
	holdScreenWait = 500 * time.Millisecond
	// holdKillLater is how much longer than termKillAfter a held terminal is
	// given before its holder is killed: the holder kills its own CLI at
	// termKillAfter, and must have the time to record that ending.
	holdKillLater = 2 * time.Second
)

const (
	// heldTyped is how many keystroke frames are remembered by sequence
	// number, so the one a holder refuses is said to the connection that
	// typed it.
	heldTyped = 1024
	// heldQuiet is how long a session may print nothing before it reads
	// quiet.
	heldQuiet = 30 * time.Second
	// holdSettle is how long a holder that has said its CLI ended is given
	// to record the ending, read every holdSettlePoll.
	holdSettle     = 10 * time.Second
	holdSettlePoll = 50 * time.Millisecond

	// The states a held terminal is described in.
	heldLive        = "live"
	heldEnded       = "ended"
	heldInterrupted = "interrupted"
)

// errHeldAway is typing, or a resize, while the link to the holder is down.
var errHeldAway = errors.New("this session's holder is not linked right now; that was not passed on")

// heldFeatures are the holder features this server uses.
var heldFeatures = []string{holdwire.FeatScreen, holdwire.FeatState, holdwire.FeatSend, holdwire.FeatStop}

/* ---------------------------------------------------------------- the link --- */

// linkHolder dials a holder, agrees a major and asks for output from the
// offset since picks, given the welcome.
func linkHolder(dir string, since func(*holdwire.WelcomeMsg) int64) (*share.Conn, *holdwire.WelcomeMsg, error) {
	c, err := net.DialTimeout("unix", holdwire.SockPath(dir), holdHandshake)
	if err != nil {
		return nil, nil, err
	}
	conn := share.NewConn(c, holdBeat)
	conn.Deadline(time.Now().Add(holdHandshake))
	fail := func(err error) (*share.Conn, *holdwire.WelcomeMsg, error) {
		conn.Close()
		return nil, nil, err
	}
	if err := conn.SendJSON(holdwire.Hello, holdwire.HelloMsg{Majors: holdwire.Majors, Features: heldFeatures}); err != nil {
		return fail(err)
	}
	kind, payload, err := conn.Read()
	if err != nil {
		return fail(err)
	}
	switch kind {
	case holdwire.Welcome:
	case holdwire.Refused:
		var no holdwire.RefusedMsg
		_ = share.Unmarshal(payload, &no)
		return fail(fmt.Errorf("the holder refused the link: %s", no.Message))
	default:
		return fail(fmt.Errorf("the holder opened with frame %d", kind))
	}
	var w holdwire.WelcomeMsg
	if err := share.Unmarshal(payload, &w); err != nil {
		return fail(err)
	}
	if _, ok := holdwire.Agree(holdwire.Majors, []int{w.Major}); !ok {
		return fail(fmt.Errorf("the holder answered in protocol %d", w.Major))
	}
	if err := conn.SendJSON(holdwire.Since, holdwire.SinceMsg{Offset: since(&w)}); err != nil {
		return fail(err)
	}
	conn.Clear()
	return conn, &w, nil
}

/* ------------------------------------------------------------- the far end --- */

// heldScreen is a holder's repaint and the offset it stands for.
type heldScreen struct {
	head    holdwire.ScreenHead
	repaint []byte
}

// typist is one keystroke frame and the connection that typed it.
type typist struct {
	seq uint64
	tc  *termConn
}

// heldTerm is a terminal whose CLI a holder process keeps.
type heldTerm struct {
	s   *Server
	ts  *termSession
	dir string
	// pid, start, ticks and boot are the holder, so that it is signalled
	// only while it is still the process that was started. proc is the same
	// process while this server is its parent, which can never be another.
	pid   int
	start time.Time
	ticks int64
	boot  string
	proc  *os.Process
	// left closes when this server lets go of the terminal.
	left     chan struct{}
	leftOnce sync.Once

	mu       sync.Mutex
	conn     *share.Conn // nil while the link is down
	features []string
	state    string
	// stopped is when this server saw the session stop.
	stopped time.Time
	title   string
	reg     holdwire.Registry
	// conv and convCwd are the conversation and the folder it is in, kept
	// once known: a registry that goes away does not take them with it.
	conv    string
	convCwd string
	// convName is the name the CLI gave the conversation, kept the same way.
	convName string

	lastOut time.Time
	seq     uint64
	typed   [heldTyped]typist
	askSeq  uint64
	asks    map[uint64]chan *heldScreen
	// sends are the sends waiting for the holder's answer, by sequence.
	sendSeq uint64
	sends   map[uint64]*sending
}

func (h *heldTerm) link() *share.Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn
}

func (h *heldTerm) has(feature string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Contains(h.features, feature)
}

// typeInto is typing from nowhere in particular; typeFrom is what a
// connection's keystrokes go through, so a refusal finds its way back.
func (h *heldTerm) typeInto(p []byte) error { return h.typeFrom(nil, p) }

func (h *heldTerm) typeFrom(tc *termConn, p []byte) error {
	h.mu.Lock()
	conn := h.conn
	if conn == nil {
		h.mu.Unlock()
		return errHeldAway
	}
	h.seq++
	seq := h.seq
	h.typed[seq%heldTyped] = typist{seq: seq, tc: tc}
	h.mu.Unlock()
	if err := conn.Send(holdwire.Input, holdwire.PutInput(seq, p)); err != nil {
		return errHeldAway
	}
	return nil
}

func (h *heldTerm) resize(cols, rows uint16) error {
	conn := h.link()
	if conn == nil || conn.SendJSON(holdwire.Resize, holdwire.SizeMsg{Cols: cols, Rows: rows}) != nil {
		return errHeldAway
	}
	return nil
}

// hangUp asks the holder to hang its CLI up. A holder that cannot be told
// over the link is told by the signal it takes to mean the same.
func (h *heldTerm) hangUp() {
	if conn := h.link(); conn != nil && h.has(holdwire.FeatStop) && conn.Send(holdwire.Stop, nil) == nil {
		return
	}
	h.signal(syscall.SIGTERM)
}

// end kills the holder. Its CLI, on a pseudo-terminal the holder kept open,
// is hung up by the kernel when the holder goes.
func (h *heldTerm) end() { h.signal(syscall.SIGKILL) }

func (h *heldTerm) close() {
	if conn := h.link(); conn != nil {
		conn.Close()
	}
}

// detach lets go of the holder and leaves it running: the link closes and
// run returns without an ending.
func (h *heldTerm) detach() {
	h.leftOnce.Do(func() { close(h.left) })
	h.close()
}

func (h *heldTerm) gone() bool {
	select {
	case <-h.left:
		return true
	default:
		return false
	}
}

// describe adds what only a held terminal has. Called with the session's mu
// held.
//
// reachable is whether the link to the holder is up now. resumable is an
// interrupted session whose conversation is known — the one Resume can
// start again on.
func (h *heldTerm) describe(doc map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	doc["held"] = true
	doc["state"] = h.state
	doc["attachable"] = true
	doc["reachable"] = h.state == heldLive && h.conn != nil
	doc["pid"] = h.pid
	doc["title"] = vtscan.CleanTitle(h.title)
	doc["conversation"] = h.conv
	doc["conversation_cwd"] = h.convCwd
	doc["resumable"] = h.state == heldInterrupted && h.conv != ""
	doc["name"] = h.name()
	doc["status"] = h.status()
	if h.state != heldLive && !h.stopped.IsZero() {
		doc["ended_at"] = h.stopped.Format(time.RFC3339)
	}
}

// name is the first of these that there is: what the person called it, the
// name the CLI gave its conversation, and its folder — the rule its record
// names it by (holdwire.Record.Name), so the daemon, the listing from the
// records and rota send all call a session one thing. The title the CLI
// sets is never its name: it changes as the CLI works, and is said beside
// the name as the title.
func (h *heldTerm) name() string {
	switch {
	case h.ts.name != "":
		return h.ts.name
	case h.convName != "":
		return h.convName
	case h.ts.cwd != "":
		return filepath.Base(h.ts.cwd)
	}
	return h.ts.id
}

// status is running, needs_you or quiet while the session lives, and stopped
// once it has ended.
func (h *heldTerm) status() string {
	switch {
	case h.state != heldLive:
		return "stopped"
	case h.reg.WaitingFor != "":
		return "needs_you"
	case h.s.now().Sub(h.lastOut) >= heldQuiet:
		return "quiet"
	}
	return "running"
}

/* ------------------------------------------------------------ its whole life --- */

// run reads the link until the CLI ends. A link lost while the holder still
// holds its lock is dialled again, from the offset this end has got to; one
// lost with the lock free is a holder that has gone, and its record says
// whether the CLI ended first.
func (h *heldTerm) run(termSink) int {
	stop := make(chan struct{})
	defer close(stop)
	go h.ping(stop)
	for {
		conn := h.link()
		if code, ended := h.read(conn); ended {
			h.setState(heldEnded)
			go h.settle()
			return code
		}
		h.mu.Lock()
		h.conn = nil
		h.mu.Unlock()
		// Closed here and not left to the collector: a link that broke at
		// this end — a silence, a frame that was not one — is still open at
		// the holder's, which would go on writing into it until the next
		// link replaced it.
		if conn != nil {
			conn.Close()
		}
		code, back := h.relink()
		// A send that went down the link that broke is answered now, once
		// it is known whether the holder is still there: a holder drops a
		// send whose link closes before it types it. One whose CLI ended
		// was answered as ended when the state was set.
		if conn != nil {
			h.mu.Lock()
			h.answerSends(conn, sentUnlinked)
			h.mu.Unlock()
		}
		if !back {
			return code
		}
	}
}

func (h *heldTerm) ping(stop chan struct{}) {
	t := time.NewTicker(holdBeat)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if conn := h.link(); conn != nil {
				_ = conn.Send(holdwire.Ping, nil)
			}
		}
	}
}

// read carries one link's frames, until it breaks or says the CLI exited.
func (h *heldTerm) read(conn *share.Conn) (code int, ended bool) {
	if conn == nil {
		return 0, false
	}
	for {
		kind, payload, err := conn.Read()
		if err != nil {
			return 0, false
		}
		switch kind {
		case holdwire.Output:
			if off, p, err := holdwire.OutputOf(payload); err == nil && len(p) > 0 {
				h.mu.Lock()
				h.lastOut = h.s.now()
				h.mu.Unlock()
				h.ts.outputAt(off, p)
			}
		case holdwire.Resized:
			var msg holdwire.SizeMsg
			if share.Unmarshal(payload, &msg) == nil {
				h.ts.resizedBy(msg.Cols, msg.Rows)
			}
		case holdwire.Exit:
			var msg holdwire.ExitMsg
			if share.Unmarshal(payload, &msg) != nil {
				return -1, true
			}
			return msg.Code, true
		case holdwire.State:
			var msg holdwire.StateMsg
			if share.Unmarshal(payload, &msg) == nil {
				h.noteState(&msg)
			}
		case holdwire.Screen:
			h.screened(payload)
		case holdwire.InputRefused:
			var msg holdwire.InputRefusedMsg
			if share.Unmarshal(payload, &msg) == nil {
				h.refused(&msg)
			}
		case holdwire.Sent:
			var msg holdwire.SentMsg
			if share.Unmarshal(payload, &msg) == nil {
				h.sent(&msg)
			}
		case holdwire.Ping:
			_ = conn.Send(holdwire.Pong, nil)
		}
	}
}

// relink dials the holder again each holdRedial while it holds its lock.
// back is false when there is no link to come back to: this server let go,
// or the holder has gone, and code is then how it ended.
func (h *heldTerm) relink() (code int, back bool) {
	for {
		if h.gone() {
			return -1, false
		}
		if !holdwire.Locked(holdwire.LockPath(h.dir)) {
			// The lock before the record: a holder records its ending and
			// then lets go, so read the other way round an ending would be
			// missed.
			m, err := holdwire.ReadMeta(h.dir)
			if err == nil && m.Exit != nil {
				h.setState(heldEnded)
				h.stoppedFrom(m)
				return *m.Exit, false
			}
			h.setState(heldInterrupted)
			if err == nil {
				h.stoppedFrom(m)
			}
			return -1, false
		}
		conn, w, err := linkHolder(h.dir, func(*holdwire.WelcomeMsg) int64 { return h.ts.end() })
		if err == nil {
			h.mu.Lock()
			h.conn, h.features = conn, w.Features
			h.mu.Unlock()
			if h.gone() {
				conn.Close() // let go of while dialling
				return -1, false
			}
			h.ts.audit("terminal relinked", "pid", h.pid)
			h.ts.resizedBy(w.Cols, w.Rows)
			return 0, true
		}
		select {
		case <-h.left:
		case <-time.After(holdRedial):
		}
	}
}

// stoppedFrom makes when the session stopped the moment its record gives,
// which is what describes it once this server stops keeping it, so
// ended_at says the same before and after. A holder that went without
// recording an ending is first stamped with the moment it was seen to go:
// that is when it stopped, as nearly as anybody can say, and its record
// says so from then on rather than the last time its holder touched it.
func (h *heldTerm) stoppedFrom(m *holdwire.Meta) {
	h.mu.Lock()
	seen := h.stopped
	h.mu.Unlock()
	if m.Exit == nil && !seen.IsZero() {
		_ = os.Chtimes(holdwire.MetaPath(h.dir), seen, seen)
	}
	if at := holdwire.StoppedAt(h.dir, m); !at.IsZero() {
		h.mu.Lock()
		h.stopped = at
		h.mu.Unlock()
	}
}

// settle waits a little for a holder that has said its CLI ended to record
// the ending, which it does a moment after, and then takes when the
// session stopped from the record.
func (h *heldTerm) settle() {
	for deadline := time.Now().Add(holdSettle); time.Now().Before(deadline); time.Sleep(holdSettlePoll) {
		if m, err := holdwire.ReadMeta(h.dir); err == nil && m.Exit != nil {
			h.stoppedFrom(m)
			return
		}
	}
}

func (h *heldTerm) setState(state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state != heldLive && h.stopped.IsZero() {
		h.stopped = h.s.now()
	}
	h.state = state
	if state != heldLive {
		h.answerSends(nil, holdwire.SentEnded)
	}
}

// noteState keeps what the holder last said about the session. A registry
// that has gone keeps the conversation it last named.
func (h *heldTerm) noteState(msg *holdwire.StateMsg) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.title = msg.Title
	if msg.Registry != nil {
		h.reg = *msg.Registry
		if msg.Registry.SessionID != "" {
			h.conv = msg.Registry.SessionID
		}
		if msg.Registry.Name != "" {
			h.convName = msg.Registry.Name
		}
		if msg.Registry.Cwd != "" {
			h.convCwd = msg.Registry.Cwd
		}
	}
}

// refused says a holder's refusal to the connection that typed the
// keystrokes, if it is one of the last heldTyped frames.
func (h *heldTerm) refused(msg *holdwire.InputRefusedMsg) {
	h.mu.Lock()
	t := h.typed[msg.Seq%heldTyped]
	h.mu.Unlock()
	if t.seq == msg.Seq && t.tc != nil {
		t.tc.fail(msg.Message)
	}
}

// screen asks the holder for its screen and waits a moment for it. Nil is
// no screen to be had: a holder without one, a link that is down, one that
// did not answer in time, or an answer with nothing to paint — a real
// repaint is never empty, and a viewer told one is coming would take the
// first bytes after it for the screen.
func (h *heldTerm) screen() *heldScreen {
	h.mu.Lock()
	conn := h.conn
	if conn == nil || !slices.Contains(h.features, holdwire.FeatScreen) {
		h.mu.Unlock()
		return nil
	}
	h.askSeq++
	seq := h.askSeq
	ch := make(chan *heldScreen, 1)
	h.asks[seq] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.asks, seq)
		h.mu.Unlock()
	}()
	if conn.SendJSON(holdwire.ScreenAsk, holdwire.ScreenAskMsg{Seq: seq}) != nil {
		return nil
	}
	select {
	case sc := <-ch:
		if sc.head.OK && len(sc.repaint) > 0 {
			return sc
		}
	case <-time.After(holdScreenWait):
	}
	return nil
}

func (h *heldTerm) screened(payload []byte) {
	head, repaint, err := holdwire.ScreenOf(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	ch := h.asks[head.Seq]
	h.mu.Unlock()
	if ch != nil {
		select {
		case ch <- &heldScreen{head: head, repaint: repaint}:
		default:
		}
	}
}
