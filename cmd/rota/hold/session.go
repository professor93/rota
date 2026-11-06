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
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/internal/vtscan"
)

// A held session is one CLI on one pty, kept by goroutines that never wait
// on each other for anything but a lock held for a copy:
//
//   - the pump reads what the CLI prints, answers its terminal queries,
//     keeps it in the ring, and wakes the others;
//   - the emulator feeds the ring to the Screen, a sequence boundary at a
//     time, and answers the cursor-position queries the Screen alone knows;
//   - the writer is the only thing that writes to the CLI: answers first,
//     then keystrokes, from a queue that refuses out loud when it is full;
//   - the link, when the daemon has one, sends the ring from wherever the
//     daemon has got to, so a daemon that is slow loses bytes it can see it
//     lost, and the CLI loses nothing;
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
	OnExit        func(code int)
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
	sendWait    = 30 * time.Second
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

// Session is one held CLI. Start makes it, Serve links it to the daemon, and
// Done says when the CLI has ended and everything about it is finished.
type Session struct {
	cfg    Config
	child  Child
	scan   *vtscan.Scanner // the pump's alone
	screen *Screen
	w      *writer

	mu      sync.Mutex
	ring    *ring.Ring
	ground  int64        // the last offset between two sequences
	gmodes  vtscan.Modes // the modes as of ground
	dues    []due
	owed    int          // dues the screen has taken and not yet answered
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
	// that the pump never takes: a repaint and a resize see the screen at a
	// sequence boundary, with the modes that were on there.
	em struct {
		mu      sync.Mutex
		off     int64
		modes   vtscan.Modes
		cols    uint16
		rows    uint16
		retired bool
	}
	emWake chan struct{}
	emDone chan struct{} // the emulator's goroutine has returned

	turn     chan struct{} // one send types at a time
	finished chan struct{} // the session is ending: the emulator stops
	exited   chan struct{} // the CLI's process is gone
	pumped   chan struct{} // nothing more will be read from it
	done     chan struct{}
	stopping sync.Once
}

// Start begins holding c, at the size cfg gives, bounded.
func Start(cfg Config, c Child) *Session {
	cols, rows := holdwire.BoundSize(cfg.Cols, cfg.Rows)
	s := &Session{
		cfg:      cfg,
		child:    c,
		scan:     vtscan.NewScanner(),
		screen:   NewScreen(int(cols), int(rows)),
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
	s.em.cols, s.em.rows = cols, rows
	s.w = newWriter(c, InputQueue, s.log)
	s.log("holding the CLI", "session", cfg.ID, "pid", c.Pid(), "cols", cols, "rows", rows)
	go s.pump()
	go s.emulate()
	go s.w.run()
	go s.life()
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

/* ----------------------------------------------------------- the emulator --- */

func (s *Session) emulate() {
	defer close(s.emDone)
	defer s.recovered("the screen")
	for {
		select {
		case <-s.emWake:
		case <-s.finished:
			return
		}
		s.safely("the screen", func() {
			s.em.mu.Lock()
			defer s.em.mu.Unlock()
			s.catchUp()
		})
	}
}

// catchUp feeds the screen the ring up to the last ground point, cutting at
// each cursor query to answer it with the cursor as of that byte. The caller
// holds s.em.mu.
//
// A screen the ring has overtaken cannot know what it missed, so it is
// retired for the session's life, said once, and the cursor is the corner
// from then on, as it is for a screen whose emulator panicked.
func (s *Session) catchUp() {
	s.mu.Lock()
	target, modes := s.ground, s.gmodes
	n := 0
	for n < len(s.dues) && s.dues[n].end <= target {
		n++
	}
	dues := make([]due, n)
	copy(dues, s.dues)
	s.dues = append(s.dues[:0], s.dues[n:]...)
	if n > 0 {
		s.owed += n
		defer func() {
			s.mu.Lock()
			s.owed -= n
			s.mu.Unlock()
		}()
	}
	live := !s.em.retired && s.screen.OK()
	var data []byte
	var from int64
	if live && target > s.em.off {
		data, from = s.ring.Read(s.em.off)
	}
	s.mu.Unlock()
	if live && from > s.em.off {
		s.em.retired, live = true, false
		s.screen.Close()
		s.log("the screen fell a whole ring behind its CLI and is retired; viewers get the ring replayed",
			"behind", from-s.em.off)
	}
	at := s.em.off
	for _, d := range dues {
		if !d.cpr {
			s.w.reply(d.replies...)
			continue
		}
		if live && d.end > at {
			s.screen.Write(data[at-from : d.end-from])
			at = d.end
		}
		row, col := s.screen.Cursor()
		s.w.reply(vtscan.CPRReply(row, col, d.private))
	}
	if live && target > at {
		s.screen.Write(data[at-from : target-from])
	}
	if target > s.em.off {
		s.em.off, s.em.modes = target, modes
	}
}

// putScreen makes a Screen payload. It is a variable so a test can make one
// the daemon could not take.
var putScreen = holdwire.PutScreen

// repaint is a Screen payload: the screen once it has caught up, the offset
// it stands for and the modes on there. OK is false with no emulator, or a
// repaint too big for a frame, and then it carries no repaint: the daemon
// replays the ring instead. Every ScreenAsk gets an answer it can take.
func (s *Session) repaint(seq uint64) []byte {
	s.em.mu.Lock()
	defer s.em.mu.Unlock()
	s.catchUp()
	h := holdwire.ScreenHead{Seq: seq, Offset: s.em.off, Cols: s.em.cols, Rows: s.em.rows, Modes: s.em.modes}
	var out []byte
	if !s.em.retired {
		out, h.OK = s.screen.Repaint(s.em.modes)
	}
	p, err := putScreen(h, out)
	if err == nil && len(p)+1 <= share.MaxFrame {
		return p
	}
	if h.OK {
		s.log("a repaint cannot be sent; the daemon replays the ring instead", "bytes", len(p), "err", err)
		h.OK = false
		if p, err = putScreen(h, nil); err == nil && len(p)+1 <= share.MaxFrame {
			return p
		}
	}
	s.log("a Screen could not be made", "err", err)
	return nil
}

// resize takes the CLI and the screen to a bounded size. Once the CLI is
// gone there is nothing to resize, and false says no answer is owed.
func (s *Session) resize(ask holdwire.SizeMsg) (holdwire.SizeMsg, bool) {
	cols, rows := holdwire.BoundSize(ask.Cols, ask.Rows)
	select {
	case <-s.exited:
		return holdwire.SizeMsg{}, false
	default:
	}
	s.em.mu.Lock()
	defer s.em.mu.Unlock()
	// What was printed at the old size is laid out at the old size.
	s.catchUp()
	if err := s.child.Resize(cols, rows); err != nil {
		s.log("resizing the CLI failed", "err", err)
		return holdwire.SizeMsg{}, false
	}
	s.screen.Resize(int(cols), int(rows))
	s.em.cols, s.em.rows = cols, rows
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return holdwire.SizeMsg{Cols: cols, Rows: rows}, true
}

/* ---------------------------------------------------------------- the end --- */

// life waits for the CLI to exit, then ends the session in order: the last
// output read, a waiting send told, the Exit offered behind the output,
// OnExit, Done.
func (s *Session) life() {
	defer s.recovered("the session's end")
	code := s.child.Wait()
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
	// Nothing feeds the screen once Done is closed. A repaint asked for
	// after this still catches up by itself.
	close(s.finished)
	select {
	case <-s.emDone:
	case <-time.After(exitFlush):
		s.log("the screen did not stop in time")
	}
	if s.cfg.OnExit != nil {
		s.safely("OnExit", func() { s.cfg.OnExit(code) })
	}
	s.log("the CLI ended", "code", code)
	close(s.done)
	s.mu.Lock()
	s.over = true
	l = s.link
	s.mu.Unlock()
	if l != nil {
		l.close()
	}
}

/* --------------------------------------------------------------- the rest --- */

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
