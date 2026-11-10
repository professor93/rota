//go:build linux || darwin

package hold

import (
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/ring"
	"github.com/professor93/rota/internal/vtscan"
)

// A held session is one CLI on one pty, kept by goroutines that never wait
// on each other for anything but a lock held for a copy:
//
//   - the pump reads what the CLI prints, answers its terminal queries,
//     keeps it in the ring, and wakes the others;
//   - the emulator feeds the ring to the Screen, a sequence boundary at a
//     time, answers the cursor-position queries the Screen alone knows, and
//     takes each size the terminal took where in the output it took it,
//     and keeps a retired screen again once the CLI is quiet;
//   - the writer is the only thing that writes to the CLI: answers first,
//     then keystrokes, from a queue that refuses out loud when it is full;
//   - the link, when the daemon has one, sends the ring from wherever the
//     daemon has got to, so a daemon that is slow loses bytes it can see it
//     lost, and the CLI loses nothing; what it asks of the screen, a repaint
//     or a resize, waits on a worker of its own, never in front of a
//     keystroke, a stop or a send;
//   - and life waits for the CLI to exit, and ends the rest in order.
//
// The pump is the one that must never stop. A CLI whose output is not read
// stops when its pty fills, whoever is or is not looking.

// Child is the CLI a session holds: its side of the pty to read and write,
// and its process to signal and wait for.
type Child interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows uint16) error
	Signal(os.Signal) error
	Wait() int
	Pid() int
	Close() error
}

// Config is what a session is started with. Terminals is the store's
// terminals directory, which says which daemon may link. Log is called from
// several goroutines at once.
type Config struct {
	ID, Terminals string
	Cols, Rows    uint16
	Scrollback    int
	Log           func(msg string, args ...any)
	// OnExit is how the CLI ended, told as soon as it has: before its last
	// output is read and before the daemon is told, so that a holder killed
	// while it finishes leaves the ending recorded.
	OnExit func(code int)
}

// The timings a test may shorten and the queue it may shrink.
var (
	// StopAfter is how long a hung-up CLI has before it is killed.
	StopAfter = 3 * time.Second
	// Beat is how often a quiet link is pinged; one silent for three beats
	// is gone.
	Beat = 10 * time.Second
	// InputQueue is how many bytes of keystrokes may wait for the CLI.
	InputQueue = 1 << 20
)

// The timings nobody outside needs.
var (
	helloWithin = 10 * time.Second       // a link's Hello
	exitDrain   = time.Second            // the output still in the pty after the CLI exits
	exitFlush   = 2 * time.Second        // the Exit frame's time to leave
	sinceGrace  = time.Second            // a new link's Since, when the CLI has just ended
	stateEvery  = 250 * time.Millisecond // at most four States a second
	quietFor    = 1500 * time.Millisecond
	sendWait    = holdwire.SendWait
	enterAfter  = 100 * time.Millisecond
)

const (
	// pumpRead is one read of the CLI's output: a burst, not a line.
	pumpRead = 32 << 10
	// maxDue bounds the answers waiting on the screen. Past it a CLI is
	// asking where its cursor is faster than anything can tell it, and is
	// told the corner at once.
	maxDue = 4096
)

// due is an answer the emulator gives once it has fed the screen up to end:
// a cursor position, or the answers to queries that came after one, which
// wait for it so that a CLI reads its answers in the order it asked.
type due struct {
	end     int64
	cpr     bool
	private bool
	replies [][]byte
}

// sized is a size the terminal took once the ring had reached end, which the
// screen takes there too: what was printed before it was printed at the
// size before.
type sized struct {
	end        int64
	cols, rows uint16
}

// Session is one held CLI. Start makes it, Serve links it to the daemon, and
// Done says when the CLI has ended and everything about it is finished.
type Session struct {
	cfg   Config
	child Child
	scan  *vtscan.Scanner // the pump's alone
	// screen is replaced only by renew, under both s.mu and em.lock, so it
	// may be read under either.
	screen *Screen
	w      *writer

	mu      sync.Mutex
	ring    *ring.Ring
	ground  int64        // the last offset between two sequences
	gmodes  vtscan.Modes // the modes as of ground
	dues    []due
	owed    int          // dues the screen has taken and not yet answered
	sizes   []sized      // sizes the screen has yet to take
	drawn   int64        // em.off as of the screen's last catch-up
	modes   vtscan.Modes // the modes as of the newest byte
	title   string
	bells   uint64
	reg     *holdwire.Registry
	gen     uint64 // counts changes to title, bells, modes and registry
	cols    uint16
	rows    uint16
	lastOut time.Time
	link    *link
	sending map[*sending]struct{}
	ending  bool // the CLI has exited: a send is answered ended
	final   bool // the ring is complete and code is known
	over    bool // Done is closed: no link is taken
	code    int

	// em is the screen's progress through the ring, under a lock of its own
	// that the pump never takes and a repaint waits for only briefly: a
	// repaint sees the screen at a sequence boundary, with the modes that
	// were on there.
	em struct {
		lock    waitLock
		off     int64
		modes   vtscan.Modes
		cols    uint16
		rows    uint16
		sizedAt int64 // where the ring was when the terminal took cols×rows
		retired bool  // no screen is kept, until renew keeps one again
		broken  bool  // its emulator panicked: retired for the session's life
	}
	emWake chan struct{}
	emDone chan struct{} // the emulator's goroutine has returned
	sizing sync.Mutex    // one resize at a time, the CLI's and the screen's in one order
	// wait is how long a repaint waits for the emulator, and renewAfter how
	// quiet the CLI must be for a retired screen to be kept again: the
	// package's values when the session was made.
	wait, renewAfter time.Duration

	turn     chan struct{} // one send types at a time
	finished chan struct{} // the session is ending: the emulator stops
	exited   chan struct{} // the CLI's process is gone
	pumped   chan struct{} // nothing more will be read from it
	done     chan struct{}
	stopping sync.Once
}

// Start begins holding c, at the size cfg gives, bounded.
func Start(cfg Config, c Child) *Session {
	s := newSession(cfg, c)
	go s.pump()
	go s.emulate()
	go s.w.run()
	go s.life()
	return s
}

// newSession is a session with nothing running yet.
func newSession(cfg Config, c Child) *Session {
	cols, rows := holdwire.BoundSize(cfg.Cols, cfg.Rows)
	s := &Session{
		cfg:      cfg,
		child:    c,
		scan:     vtscan.NewScanner(),
		ring:     ring.New(cfg.Scrollback),
		cols:     cols,
		rows:     rows,
		lastOut:  time.Now(),
		code:     -1,
		sending:  map[*sending]struct{}{},
		emWake:   make(chan struct{}, 1),
		emDone:   make(chan struct{}),
		finished: make(chan struct{}),
		turn:     make(chan struct{}, 1),
		exited:   make(chan struct{}),
		pumped:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.em.lock, s.em.cols, s.em.rows = make(waitLock, 1), cols, rows
	s.wait, s.renewAfter = repaintWait, renewAfter
	if holdwire.ScreenFits(cols, rows) {
		s.screen = NewScreen(int(cols), int(rows))
	} else {
		s.screen, s.em.retired = noScreen(), true
		s.log(tooLarge, "cols", cols, "rows", rows, "cells", holdwire.MaxScreenCells)
	}
	s.w = newWriter(c, InputQueue, s.log)
	s.log("holding the CLI", "session", cfg.ID, "pid", c.Pid(), "cols", cols, "rows", rows)
	return s
}

// Done is closed once the CLI has ended, its exit has been offered to the
// daemon and OnExit has returned.
func (s *Session) Done() <-chan struct{} { return s.done }

// Code is how the CLI ended, once Done is closed; -1 before.
func (s *Session) Code() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

// SetRegistry is what the CLI's own registry says about the session now.
// It decides when a send may type, and the daemon is told it in a State.
func (s *Session) SetRegistry(r *holdwire.Registry) {
	var c *holdwire.Registry
	if r != nil {
		v := *r
		c = &v
	}
	s.mu.Lock()
	same := (c == nil && s.reg == nil) || (c != nil && s.reg != nil && *c == *s.reg)
	if !same {
		s.reg = c
		s.gen++
	}
	l := s.link
	s.mu.Unlock()
	if l != nil && !same {
		l.nudge()
	}
}

// Stop hangs up the CLI, and kills it StopAfter later if it is still there.
// A CLI that ignores its hang-up must never outlive its holder, which the
// daemon kills two seconds after that.
func (s *Session) Stop() {
	s.stopping.Do(func() {
		s.log("hanging up the CLI")
		_ = s.child.Signal(syscall.SIGHUP)
		after := StopAfter
		go func() {
			t := time.NewTimer(after)
			defer t.Stop()
			select {
			case <-s.exited:
			case <-t.C:
				s.log("the CLI is still there after its hang-up; killing it", "after", after)
				_ = s.child.Signal(syscall.SIGKILL)
			}
		}()
	})
}

// stopOnSignals makes the holder's own SIGHUP and SIGTERM a Stop, until the
// session is done or the returned function is called.
func (s *Session) stopOnSignals() (cancel func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)
	quit := make(chan struct{})
	go func() {
		defer signal.Stop(ch)
		for {
			select {
			case sig := <-ch:
				s.log("stopping on a signal", "signal", sig.String())
				s.Stop()
			case <-s.done:
				return
			case <-quit:
				return
			}
		}
	}()
	return sync.OnceFunc(func() { close(quit) })
}

/* --------------------------------------------------------------- the pump --- */

// pump reads the CLI until its terminal ends.
func (s *Session) pump() {
	defer close(s.pumped)
	defer s.recovered("the pump")
	buf := make([]byte, pumpRead)
	for {
		n, err := s.child.Read(buf)
		if n > 0 {
			s.safely("reading the CLI", func() { s.took(buf[:n]) })
		}
		if err != nil {
			return
		}
	}
}

// took is one read of output: answered, kept and announced. It waits on
// nothing but s.mu, which nobody holds for longer than a copy.
func (s *Session) took(p []byte) {
	res := s.scan.Feed(p)
	modes := s.scan.Modes()
	var now [][]byte // answers that need not wait for the screen
	s.mu.Lock()
	base := s.ring.End()
	s.ring.Write(p)
	s.lastOut = time.Now()
	if res.Ground >= 0 {
		// Modes change only where a sequence ends, which is a ground
		// point, so the modes after the read are the modes at its last.
		s.ground, s.gmodes = base+int64(res.Ground), modes
	}
	// An answer the screen still owes holds back every answer after it.
	switch {
	case len(res.CPR) == 0 && len(s.dues) == 0 && s.owed == 0:
		now = res.Replies
	case len(s.dues)+len(res.CPR) >= maxDue:
		for _, c := range res.CPR {
			now = append(now, vtscan.CPRReply(1, 1, c.Private))
		}
		now = append(now, res.Replies...)
	default:
		for _, c := range res.CPR {
			s.dues = append(s.dues, due{end: base + int64(c.End), cpr: true, private: c.Private})
		}
		if len(res.Replies) > 0 {
			// After this read's last cursor query, which is where a CLI
			// asking for both puts its sentinel.
			s.dues = append(s.dues, due{end: base + int64(max(res.Ground, 0)), replies: res.Replies})
		}
	}
	changed := res.ModesChanged
	s.modes = modes
	for _, e := range res.Events {
		switch e.Kind {
		case vtscan.EventTitle:
			changed = changed || e.Text != s.title
			s.title = e.Text
		case vtscan.EventBell:
			s.bells++
			changed = true
		}
	}
	if changed {
		s.gen++
	}
	l := s.link
	s.mu.Unlock()
	s.w.reply(now...)
	nudge(s.emWake)
	if l != nil {
		l.nudge()
	}
}

/* ---------------------------------------------------------------- the end --- */

// life waits for the CLI to exit, then ends the session in order: OnExit,
// a waiting send told, the last output read, the Exit offered behind the
// output, Done.
func (s *Session) life() {
	defer s.end()
	defer s.recovered("the session's end")
	code := s.child.Wait()
	// Recorded before anything else, all of which may take a while: a
	// holder killed from here on leaves the session ended, not
	// interrupted.
	if s.cfg.OnExit != nil {
		s.safely("OnExit", func() { s.cfg.OnExit(code) })
	}
	s.mu.Lock()
	s.ending = true
	waiting := make([]*sending, 0, len(s.sending))
	for x := range s.sending {
		waiting = append(waiting, x)
	}
	s.mu.Unlock()
	close(s.exited)
	// Told before the Exit goes, and not waited on past the Exit's own
	// allowance: a link that has stopped reading takes neither.
	told := make(chan struct{})
	go func() {
		defer close(told)
		for _, x := range waiting {
			x.answer(holdwire.SentEnded, sentEnded)
		}
	}()
	select {
	case <-told:
	case <-time.After(exitFlush):
	}
	select {
	case <-s.pumped:
	case <-time.After(exitDrain):
		s.log("the CLI has exited and something still holds its terminal; closing it")
		_ = s.child.Close()
		select {
		case <-s.pumped:
		case <-time.After(exitDrain):
			s.log("the CLI's terminal did not end when closed")
		}
	}
	s.mu.Lock()
	s.final, s.code = true, code
	l := s.link
	s.mu.Unlock()
	if l != nil {
		l.nudge()
		select {
		case <-l.exitSent:
		case <-l.gone:
		case <-time.After(exitFlush):
			s.log("the daemon did not take the exit in time")
		}
	}
	_ = s.child.Close()
	s.w.stop()
	// Nothing feeds the screen from here, and it is closed before Done, so
	// neither its emulator nor the goroutine reading the emulator's answers
	// outlives the session. A repaint asked for after this answers OK=false,
	// and the daemon has the Exit by now anyway.
	close(s.finished)
	select {
	case <-s.emDone:
		s.screenNow().Close()
	case <-time.After(exitFlush):
		s.log("the screen did not stop in time")
		go s.screenNow().Close()
	}
	s.log("the CLI ended", "code", code)
}

// end is the last of life, also when life panicked: Done closes, no link is
// taken, and the one there is closes. A holder whose end went wrong still
// ends, and then nothing feeds the screen or writes to the CLI.
func (s *Session) end() {
	select {
	case <-s.finished:
	default:
		// life panicked before it got this far.
		_ = s.child.Close()
		s.w.stop()
		close(s.finished)
		go s.screenNow().Close()
	}
	close(s.done)
	s.mu.Lock()
	s.over = true
	l := s.link
	s.mu.Unlock()
	if l != nil {
		l.close()
	}
}

/* --------------------------------------------------------------- the rest --- */

// screenNow is the session's Screen: renew replaces a retired one, and once
// finished is closed it is the last.
func (s *Session) screenNow() *Screen {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.screen
}

func (s *Session) log(msg string, args ...any) {
	if s.cfg.Log != nil {
		s.cfg.Log(msg, args...)
	}
}

// safely runs one turn of a loop, so that a panic costs that turn and is
// logged, and the loop goes on.
func (s *Session) safely(what string, f func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log("a panic in "+what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			ok = false
		}
	}()
	f()
	return true
}

// recovered is deferred at the top of every goroutine, for whatever
// escapes its loop.
func (s *Session) recovered(what string) {
	if r := recover(); r != nil {
		s.log("a panic in "+what, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	}
}

// nudge wakes whoever waits on ch, without ever waiting itself.
func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
