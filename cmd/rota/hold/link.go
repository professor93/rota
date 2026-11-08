//go:build linux || darwin

package hold

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/proc"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/wire"
)

// The sentences a link is refused or answered with.
const (
	refusedNotDaemon = "only the rota daemon serving this store may link to a session"
	refusedTooFast   = "the session is not taking keystrokes this fast; that input was not typed"
)

// features is what this holder offers on top of its major.
var features = []string{holdwire.FeatScreen, holdwire.FeatState, holdwire.FeatSend, holdwire.FeatStop}

// outChunk is the most output one Output frame carries.
const outChunk = 64 << 10

// link is the session's one connection to the daemon.
type link struct {
	s        *Session
	c        *share.Conn
	wake     chan struct{}
	gone     chan struct{} // closed once the link is over
	exitSent chan struct{} // closed once the Exit has been written
	ending   sync.Once

	// pos is where the daemon has got to in the ring, -1 until it says
	// where to start. It is under s.mu.
	pos int64
	// sent is the State generation the daemon last heard. Only the
	// goroutine pouring the link touches it, once the link is up.
	sent uint64
}

func (l *link) nudge() { nudge(l.wake) }

// close ends the link, and makes it no longer the session's.
func (l *link) close() {
	l.ending.Do(func() {
		close(l.gone)
		_ = l.c.Close()
		l.s.mu.Lock()
		if l.s.link == l {
			l.s.link = nil
		}
		l.s.mu.Unlock()
	})
}

func (l *link) send(kind byte, p []byte) error { return l.c.Send(kind, p) }

func (l *link) sendJSON(kind byte, v any) error { return l.c.SendJSON(kind, v) }

// Serve takes links from ln until the session is done, then closes it.
func (s *Session) Serve(ln *net.UnixListener) {
	go func() {
		<-s.done
		_ = ln.Close()
	}()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.log("accepting a link failed", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.take(c)
	}
}

// take is one connection, from its Hello to its end.
//
// Only the process holding the store's daemon lock may link, and the kernel
// says which process is at the other end: nothing the peer sends can change
// it. So that is asked first, before anything the peer sent is read, and
// any other process is refused at once rather than read up to a frame's
// size and given helloWithin to send it. A valid link replaces the one
// before, which is closed, so a daemon that restarted takes its sessions
// back without waiting for the old link to time out.
func (s *Session) take(c *net.UnixConn) {
	defer s.recovered("a link")
	k := share.NewConn(c, Beat)
	k.Deadline(time.Now().Add(helloWithin))
	refuse := func(why string) {
		_ = k.SendJSON(holdwire.Refused, holdwire.RefusedMsg{Message: why})
		_ = k.Close()
	}
	if pid, ok := s.fromDaemon(c); !ok {
		s.log("refused a link from a process that is not the daemon", "pid", pid)
		refuse(refusedNotDaemon)
		return
	}
	kind, payload, err := k.Read()
	if err != nil || kind != holdwire.Hello {
		_ = k.Close()
		return
	}
	var hello holdwire.HelloMsg
	if err := share.Unmarshal(payload, &hello); err != nil {
		refuse("this session's holder cannot read that hello")
		return
	}
	major, ok := holdwire.Agree(holdwire.Majors, hello.Majors)
	if !ok {
		refuse(fmt.Sprintf("this session's holder speaks holder protocol %v, and the daemon none of it", holdwire.Majors))
		return
	}

	l := &link{
		s: s, c: k, pos: -1,
		wake: make(chan struct{}, 1), gone: make(chan struct{}), exitSent: make(chan struct{}),
	}
	s.mu.Lock()
	if s.over {
		s.mu.Unlock()
		refuse("this session has ended")
		return
	}
	old := s.link
	s.link = l
	welcome := holdwire.WelcomeMsg{
		Major: major, Features: features, Version: wire.Version,
		Oldest: s.ring.Oldest(), End: s.ring.End(), Cols: s.cols, Rows: s.rows,
	}
	state := s.stateLocked()
	l.sent = s.gen
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	if k.SendJSON(holdwire.Welcome, welcome) != nil {
		l.close()
		return
	}
	k.Clear()
	if k.SendJSON(holdwire.State, state) != nil {
		l.close()
		return
	}
	go s.pour(l)
	go s.beat(l)
	s.read(l)
}

// fromDaemon says whether c's peer is the process holding the store's
// daemon lock, and which process it is. The kernel names the peer, so it is
// running now, and daemon.pid need only name it: its start is not asked
// about, which a clock set since the daemon started would make look like
// another process's (holdwire.IsDaemon).
func (s *Session) fromDaemon(c *net.UnixConn) (int, bool) {
	pid, err := proc.PeerPID(c)
	if err != nil {
		return 0, false
	}
	return pid, holdwire.IsDaemon(s.cfg.Terminals, pid)
}

// read handles the daemon's frames until the link ends. Frames it does not
// know are skipped: a newer daemon may send them.
//
// What needs the screen, a ScreenAsk or a Resize, goes to the link's own
// worker, so that keystrokes, a Stop, a send, a Since and a Ping are handled
// the moment they arrive, however busy the emulator is.
func (s *Session) read(l *link) {
	defer l.close()
	work := make(chan frame, screenQueue)
	go s.screens(l, work)
	for {
		kind, payload, err := l.c.Read()
		if err != nil {
			return
		}
		s.safely("a frame from the daemon", func() { s.handle(l, work, kind, payload) })
	}
}

func (s *Session) handle(l *link, work chan<- frame, kind byte, payload []byte) {
	switch kind {
	case holdwire.Since:
		var m holdwire.SinceMsg
		if share.Unmarshal(payload, &m) != nil {
			return
		}
		s.mu.Lock()
		l.pos = min(max(m.Offset, s.ring.Oldest()), s.ring.End())
		s.mu.Unlock()
		l.nudge()
	case holdwire.Input:
		seq, p, err := holdwire.InputOf(payload)
		if err != nil || len(p) == 0 {
			return
		}
		if !s.w.typeIn(p) {
			_ = l.sendJSON(holdwire.InputRefused, holdwire.InputRefusedMsg{Seq: seq, Message: refusedTooFast})
		}
	case holdwire.Resize, holdwire.ScreenAsk:
		select {
		case work <- frame{kind, payload}:
		default:
			s.tooMany(l, frame{kind, payload})
		}
	case holdwire.Stop:
		s.Stop()
	case holdwire.Send:
		var m holdwire.SendMsg
		if share.Unmarshal(payload, &m) != nil {
			return
		}
		// Enlisted here, in frame order, so that a send handled before
		// the CLI exits is told it ended before the Exit goes.
		if x := s.enlist(l, m.Seq); x != nil {
			go s.send(x, m)
		}
	case holdwire.Ping:
		_ = l.send(holdwire.Pong, nil)
	}
}

// frame is one frame from the daemon, kept for the screen's worker.
type frame struct {
	kind    byte
	payload []byte
}

// screenQueue is how many frames may wait for the screen on one link. The
// daemon asks for a screen twice when somebody attaches or falls behind, and
// resizes when somebody fits: a few at a time.
const screenQueue = 64

// screens is a link's worker for the screen: its ScreenAsks and Resizes, one
// at a time in the order they came, until the link ends.
func (s *Session) screens(l *link, work <-chan frame) {
	defer s.recovered("a link's screen")
	for {
		select {
		case <-l.gone:
			return
		case f := <-work:
			s.safely("a frame for the screen", func() { s.screenFrame(l, f) })
		}
	}
}

func (s *Session) screenFrame(l *link, f frame) {
	switch f.kind {
	case holdwire.Resize:
		var m holdwire.SizeMsg
		if share.Unmarshal(f.payload, &m) != nil {
			return
		}
		if got, ok := s.resize(m); ok {
			_ = l.sendJSON(holdwire.Resized, got)
		}
	case holdwire.ScreenAsk:
		var m holdwire.ScreenAskMsg
		if share.Unmarshal(f.payload, &m) != nil {
			return
		}
		if out := s.repaint(m.Seq); out != nil {
			_ = l.send(holdwire.Screen, out)
		}
	}
}

// tooMany answers a frame the worker has no room for: a ScreenAsk with no
// screen, so the daemon replays the ring, and a Resize not at all, so the
// size stays what it was and the daemon says no new one.
func (s *Session) tooMany(l *link, f frame) {
	switch f.kind {
	case holdwire.ScreenAsk:
		var m holdwire.ScreenAskMsg
		if share.Unmarshal(f.payload, &m) != nil {
			return
		}
		if out := s.unpainted(m.Seq); out != nil {
			_ = l.send(holdwire.Screen, out)
		}
	case holdwire.Resize:
		s.log("a resize was not done: too many frames wait for the screen", "waiting", screenQueue)
	}
}

// pour is everything the holder sends unasked: the ring from where the
// daemon has got to, a State when something changed and a quarter second
// has passed, and last the Exit, behind every byte of output.
//
// It reads the ring by offset, so however slow the daemon, the pump never
// waits for it: what the ring dropped meanwhile shows as a jump in the next
// Output's offset.
func (s *Session) pour(l *link) {
	defer s.recovered("a link's output")
	defer l.close()
	last := time.Now()
	var timer *time.Timer
	var later <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	graced := false
	for {
		s.mu.Lock()
		var data []byte
		var from int64
		if l.pos >= 0 && l.pos < s.ring.End() {
			data, from = s.ring.Read(l.pos)
			l.pos = from + int64(len(data))
		}
		started := l.pos >= 0
		gen, final, code := s.gen, s.final, s.code
		var state holdwire.StateMsg
		if gen != l.sent {
			state = s.stateLocked()
		}
		s.mu.Unlock()
		for len(data) > 0 {
			n := min(len(data), outChunk)
			if l.send(holdwire.Output, holdwire.PutOutput(from, data[:n])) != nil {
				return
			}
			from, data = from+int64(n), data[n:]
		}
		if gen != l.sent {
			if wait := stateEvery - time.Since(last); final || wait <= 0 {
				if l.sendJSON(holdwire.State, state) != nil {
					return
				}
				l.sent, last = gen, time.Now()
			} else if later == nil {
				timer = time.NewTimer(wait)
				later = timer.C
			}
		}
		if final && !started && !graced {
			// The CLI ended between the Welcome and the daemon saying
			// where to start: its Since is a moment away, and the last
			// output is worth that moment.
			graced = true
			if !s.awaitSince(l) {
				return
			}
			continue
		}
		if final {
			// The ring is complete, and all of it from pos on has been
			// sent: the Exit is the last thing this link carries.
			if l.sendJSON(holdwire.Exit, holdwire.ExitMsg{Code: code}) != nil {
				return
			}
			close(l.exitSent)
			<-l.gone
			return
		}
		select {
		case <-l.wake:
		case <-later:
			later = nil
		case <-l.gone:
			return
		}
	}
}

// awaitSince waits at most sinceGrace for the daemon to say where its
// output starts, and says false if the link ended meanwhile.
func (s *Session) awaitSince(l *link) bool {
	t := time.NewTimer(sinceGrace)
	defer t.Stop()
	for {
		s.mu.Lock()
		started := l.pos >= 0
		s.mu.Unlock()
		if started {
			return true
		}
		select {
		case <-l.wake:
		case <-t.C:
			return true
		case <-l.gone:
			return false
		}
	}
}

// beat pings the daemon every Beat. The link's own deadline ends it after
// three beats with nothing heard.
func (s *Session) beat(l *link) {
	defer s.recovered("a link's beat")
	t := time.NewTicker(Beat)
	defer t.Stop()
	for {
		select {
		case <-l.gone:
			return
		case <-t.C:
			if l.send(holdwire.Ping, nil) != nil {
				l.close()
				return
			}
		}
	}
}

// stateLocked is the State now. The caller holds s.mu.
func (s *Session) stateLocked() holdwire.StateMsg {
	m := holdwire.StateMsg{Title: s.title, Bells: s.bells, Modes: s.modes}
	if s.reg != nil {
		r := *s.reg
		m.Registry = &r
	}
	return m
}
