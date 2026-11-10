//go:build linux || darwin

package hold

import (
	"bytes"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/internal/vtscan"
)

/* ----------------------------------------------------------- the emulator --- */

// waitLock is a lock that can be waited for a while and then given up on: a
// channel of one, whose waiters take it in the order they came.
type waitLock chan struct{}

func (l waitLock) lock()   { l <- struct{}{} }
func (l waitLock) unlock() { <-l }

func (l waitLock) tryLock() bool {
	select {
	case l <- struct{}{}:
		return true
	default:
		return false
	}
}

// lockWithin takes the lock if it comes free within d, and says whether it
// did.
func (l waitLock) lockWithin(d time.Duration) bool {
	if l.tryLock() {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case l <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

// emulate is the emulator's goroutine: woken by output and by sizes, and,
// while a retired screen waits for a quiet moment, by a timer.
func (s *Session) emulate() {
	defer close(s.emDone)
	defer s.recovered("the screen")
	later := time.NewTimer(time.Hour)
	later.Stop()
	defer later.Stop()
	for {
		select {
		case <-s.emWake:
		case <-later.C:
		case <-s.finished:
			return
		}
		var wait time.Duration
		s.safely("the screen", func() {
			s.em.lock.lock()
			defer s.em.lock.unlock()
			s.catchUp(false)
			wait = s.renew()
		})
		if wait > 0 {
			later.Reset(wait)
		}
	}
}

// catchUp feeds the screen the ring up to the last ground point, cutting at
// each cursor query to answer it with the cursor as of that byte, and at each
// size the terminal took to take it there. The pieces are one batch, and
// share one budget of repeats. The caller holds s.em.lock.
//
// A limited catch-up is a repaint's: it draws nothing, and says false, when
// there is more to draw than drawLimit allows at the largest size the
// screen takes on the way.
//
// A screen the ring has overtaken cannot know what it missed, so it is
// retired, said once, and the cursor is the corner until renew keeps it
// again, as it is for good for a screen whose emulator panicked.
func (s *Session) catchUp(limited bool) bool {
	defer s.noticeLoss()
	b, ok := s.batch(limited)
	if !ok {
		return false
	}
	// Registered once s.mu is let go, so that a panic anywhere in catchUp
	// unwinds through it without waiting on a lock it holds itself.
	defer s.drew(len(b.dues))
	target, modes, dues, sizes, live, data, from := b.target, b.modes, b.dues, b.sizes, b.live, b.data, b.from
	if live && from > s.em.off {
		s.em.retired, live = true, false
		s.screen.Close()
		s.log("the screen fell a whole ring behind its CLI and is retired; viewers get the ring replayed until it is quiet",
			"behind", from-s.em.off)
	}
	if live {
		s.screen.Refill()
	}
	at := s.em.off
	feed := func(end int64) {
		if live && end > at {
			s.screen.Write(data[at-from : end-from])
			at = end
		}
	}
	for len(dues) > 0 || len(sizes) > 0 {
		// A query asked where the terminal changed size was asked before it
		// did, and is answered at the size before.
		if len(sizes) > 0 && (len(dues) == 0 || sizes[0].end < dues[0].end) {
			feed(sizes[0].end)
			s.sized(sizes[0])
			live = live && !s.em.retired
			sizes = sizes[1:]
			continue
		}
		d := dues[0]
		dues = dues[1:]
		if !d.cpr {
			s.w.reply(d.replies...)
			continue
		}
		feed(d.end)
		row, col := s.screen.Cursor()
		s.w.reply(vtscan.CPRReply(row, col, d.private))
	}
	feed(target)
	if target > s.em.off {
		s.em.off, s.em.modes = target, modes
	}
	return true
}

// catchBatch is what one catch-up has to do: draw the ring up to target,
// with the modes on there, answering dues and taking sizes on the way.
type catchBatch struct {
	target int64
	modes  vtscan.Modes
	dues   []due
	sizes  []sized
	live   bool
	data   []byte
	from   int64
}

// batch takes what the screen owes up to the last ground point, under s.mu,
// and counts its dues as owed, last, once nothing more can go wrong. A
// limited batch takes nothing, and says false, when it has more to draw than
// drawLimit allows at the largest size it takes: it is counted under the same
// hold as it is read, so a read of output landing meanwhile is counted too.
// The caller holds s.em.lock.
func (s *Session) batch(limited bool) (b catchBatch, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b.target, b.modes = s.ground, s.gmodes
	n := 0
	for n < len(s.dues) && s.dues[n].end <= b.target {
		n++
	}
	k := 0
	for k < len(s.sizes) && s.sizes[k].end <= b.target {
		k++
	}
	b.live = !s.em.retired && s.screen.OK()
	if limited && b.live {
		cells := int64(s.em.cols) * int64(s.em.rows)
		for _, z := range s.sizes[:k] {
			cells = max(cells, int64(z.cols)*int64(z.rows))
		}
		if b.target-s.em.off > drawLimit(cells) {
			return catchBatch{}, false
		}
	}
	b.dues = make([]due, n)
	copy(b.dues, s.dues)
	b.sizes = make([]sized, k)
	copy(b.sizes, s.sizes)
	if b.live && b.target > s.em.off {
		b.data, b.from = s.ring.Read(s.em.off)
	}
	s.dues = append(s.dues[:0], s.dues[n:]...)
	s.sizes = append(s.sizes[:0], s.sizes[k:]...)
	s.owed += n
	return b, true
}

// drew is the end of a catch-up: the n dues it took are answered, and how
// far the screen has drawn is known without its lock.
func (s *Session) drew(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owed -= n
	s.drawn = s.em.off
}

// tooLarge is what the log says of a terminal larger than a screen is kept
// for.
const tooLarge = "the terminal is larger than a screen is kept for; viewers get the ring replayed until it is made smaller"

// sized takes the screen to the size the terminal took where the ring had
// reached z.end, or retires it for a size larger than a screen is kept for.
// A fit to the size it already has changes nothing, and is not where the
// size was taken: the CLI is sent no SIGWINCH for it and draws nothing. The
// caller holds s.em.lock.
func (s *Session) sized(z sized) {
	if z.cols != s.em.cols || z.rows != s.em.rows {
		s.em.sizedAt = z.end
	}
	s.em.cols, s.em.rows = z.cols, z.rows
	if s.em.retired {
		return
	}
	if !holdwire.ScreenFits(z.cols, z.rows) {
		s.em.retired = true
		s.screen.Close()
		s.log(tooLarge, "cols", z.cols, "rows", z.rows, "cells", holdwire.MaxScreenCells)
		return
	}
	s.screen.Resize(int(z.cols), int(z.rows))
}

// noticeLoss retires the screen for the session's life once its emulator
// has panicked, and says so in one line with what it panicked with: the
// Screen keeps the holder up, and the log says why viewers get the ring
// replayed. A fresh emulator would be fed the same CLI's output, so renew
// leaves it retired. The caller holds s.em.lock.
func (s *Session) noticeLoss() {
	if s.em.retired {
		return
	}
	if why := s.screen.Lost(); why != "" {
		s.em.retired, s.em.broken = true, true
		s.log("the screen's emulator panicked and is retired; viewers get the ring replayed", "panic", why)
	}
}

// renewAfter is how long the CLI must have printed nothing before a retired
// screen is kept again: a moment, so that a fresh one is not started in the
// middle of the flood that overtook the last.
var renewAfter = time.Second

// renew keeps a retired screen again once it can be: when its emulator did
// not panic, the terminal is within the budget, and the CLI has been quiet
// for renewAfter. It says how long until it may be, or 0.
//
// The fresh screen knows only what it is fed, and nobody asks the CLI to
// draw again: a SIGWINCH with no change of size is nothing to a CLI on Node,
// which redraws only when the size it reads has changed, and a resize there
// and back would have it draw twice, once at a size nobody asked for, in
// front of everybody watching. So the fresh screen starts on the last of the
// ring, as a viewer replayed the ring sees it (renewFrom says where). What
// the CLI drew in answer to the size that fits, when it took that size a
// moment ago, is all there. The caller holds s.em.lock.
func (s *Session) renew() time.Duration {
	if !s.em.retired || s.em.broken || !holdwire.ScreenFits(s.em.cols, s.em.rows) {
		return 0
	}
	cols, rows := s.em.cols, s.em.rows
	start, alt, wait, ok := s.renewFrom(drawLimit(int64(cols) * int64(rows)))
	if !ok {
		return wait
	}
	// Made without s.mu, which the pump takes for every read: at the budget
	// an emulator is 52 MB.
	fresh := newScreen(int(cols), int(rows))
	if alt {
		fresh.Write([]byte("\x1b[?1049h"))
	}
	if !fresh.OK() {
		s.em.broken = true
		s.log("the screen's emulator panicked and is retired; viewers get the ring replayed", "panic", fresh.Lost())
		return 0
	}
	s.mu.Lock()
	select {
	case <-s.finished:
		// life closes the screen it finds once finished is closed.
		s.mu.Unlock()
		fresh.Close()
		return 0
	default:
	}
	s.screen, s.drawn = fresh, start
	s.mu.Unlock()
	s.em.retired, s.em.off = false, start
	s.log("the screen is kept again", "cols", cols, "rows", rows, "from", start)
	nudge(s.emWake)
	return 0
}

// newScreen is how renew makes a fresh screen. It is a variable so a test
// can end the session while it is made.
var newScreen = NewScreen

// renewFrom is where a fresh screen starts, and whether the CLI was on its
// alternate screen there; or, while the CLI has not been quiet for long
// enough, how long until it has. ok is false with no wait when there is
// nowhere to start yet: the next output wakes the emulator. The caller holds
// s.em.lock.
//
// The start is no further back than the oldest byte the ring keeps, than
// where the terminal took its size, since what came before was printed for
// another size, or than limit bytes before the last ground point, what the
// emulator draws in a moment. From there it moves on to where nothing that
// came before can matter:
//   - just past the last switch of the alternate screen in what is left,
//     where the screen in use is the one the CLI is on now; otherwise
//   - where the terminal took its size, or where the CLI began; otherwise
//   - the first ESC, which begins a sequence whatever came before, and
//     ends any string (an OSC, a DCS) the start was inside; otherwise
//   - just past the last BEL, CAN or SUB, which end a string too; otherwise
//   - just past the first newline.
//
// With nothing left to draw from there, a fresh screen would be empty where
// a replayed viewer sees the CLI, so it waits for the CLI to print. So does
// a string the CLI has not finished, longer than the ring: the last ground
// point is where it began, older than the oldest byte kept, and the ring
// has nothing between two sequences to start on.
func (s *Session) renewFrom(limit int64) (start int64, alt bool, wait time.Duration, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.finished:
		return 0, false, 0, false
	default:
	}
	if wait := s.renewAfter - time.Since(s.lastOut); wait > 0 {
		return 0, false, wait, false
	}
	ground := s.ground
	start = max(s.ring.Oldest(), s.em.sizedAt, ground-limit)
	if start < ground {
		data, from := s.ring.Read(start)
		tail := data[:ground-from]
		if i := pastAltSwitch(tail); i >= 0 {
			start = from + int64(i)
		} else if start != s.em.sizedAt && start != 0 {
			i := bytes.IndexByte(tail, 0x1b)
			if i < 0 {
				// With no ESC, nothing in the tail opens a string, so one
				// the start is inside ends at a BEL, CAN or SUB: past the
				// last of them is outside it. A DCS may hold a BEL before
				// the CAN that ends it, so it is the last.
				if i = bytes.LastIndexAny(tail, "\x07\x18\x1a"); i < 0 {
					i = bytes.IndexByte(tail, '\n')
				}
				if i >= 0 {
					i++
				}
			}
			if i < 0 {
				return 0, false, 0, false
			}
			start = from + int64(i)
		}
	}
	if start >= ground {
		return 0, false, 0, false
	}
	return start, s.gmodes.AltScreen, 0, true
}

// altSwitches are the ways a CLI switches between the main and the
// alternate screen, as vtscan reads them on their own.
var altSwitches = [][]byte{
	[]byte("\x1b[?1049h"), []byte("\x1b[?1049l"),
	[]byte("\x1b[?1047h"), []byte("\x1b[?1047l"),
	[]byte("\x1b[?47h"), []byte("\x1b[?47l"),
}

// pastAltSwitch is the index just past the last switch of the alternate
// screen in p, or -1 for none.
func pastAltSwitch(p []byte) int {
	at := -1
	for _, sw := range altSwitches {
		if i := bytes.LastIndex(p, sw); i >= 0 {
			at = max(at, i+len(sw))
		}
	}
	return at
}

// putScreen makes a Screen payload. It is a variable so a test can make one
// the daemon could not take.
var putScreen = holdwire.PutScreen

// maxBehind is how far the screen may be behind the CLI for a repaint to
// wait for it at all: one read of output. Further behind, catching up could
// take minutes at a large size, and the daemon has replayed the ring long
// before.
const maxBehind = pumpRead

// repaintWait is how long a repaint waits for the emulator to finish what it
// is drawing. The daemon waits half a second for an answer, and a fit asked
// for after the repaint waits behind it. A variable so a test can make the
// wait as long as the screen is held.
var repaintWait = 200 * time.Millisecond

// behind says whether the screen is more than limit bytes behind the CLI,
// as of its last catch-up.
func (s *Session) behind(limit int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ground-s.drawn > limit
}

// drawLimit is the most a repaint draws itself before it answers: what the
// emulator draws in about a third of a second at its slowest, a line feed
// at the bottom of the screen for each byte, which costs it about five
// nanoseconds a cell. That is one read of output at 80×24, and less on a
// larger screen.
func drawLimit(cells int64) int64 {
	cells = max(cells, 1)
	return min(maxBehind, maxBehind*80*24/cells)
}

// repaint is a Screen payload: the screen once it has caught up, the offset
// it stands for and the modes on there. OK is false with no emulator, a
// screen too far behind to wait for, or a repaint too big for a frame, and
// then it carries no repaint: the daemon replays the ring instead. Every
// ScreenAsk gets an answer it can take.
//
// A repaint never waits long: a flood at a large size can hold the emulator
// for minutes. A screen far behind is answered at once; one a little behind
// is waited for repaintWait at most; and a repaint draws no more of the
// backlog itself than drawLimit allows at the largest size it takes on the
// way.
func (s *Session) repaint(seq uint64) []byte {
	if !s.em.lock.tryLock() && (s.behind(maxBehind) || !s.em.lock.lockWithin(s.wait)) {
		return s.unpainted(seq)
	}
	defer s.em.lock.unlock()
	if !s.catchUp(true) {
		return s.unpainted(seq)
	}
	h := holdwire.ScreenHead{Seq: seq, Offset: s.em.off, Cols: s.em.cols, Rows: s.em.rows, Modes: s.em.modes}
	var out []byte
	if !s.em.retired {
		out, h.OK = s.screen.Repaint(s.em.modes)
		s.noticeLoss()
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

// unpainted is a Screen with no repaint, made without the emulator: where the
// screen has drawn to and the terminal's size.
func (s *Session) unpainted(seq uint64) []byte {
	s.mu.Lock()
	h := holdwire.ScreenHead{Seq: seq, Offset: s.drawn, Cols: s.cols, Rows: s.rows}
	s.mu.Unlock()
	p, err := putScreen(h, nil)
	if err != nil {
		s.log("a Screen could not be made", "err", err)
		return nil
	}
	return p
}

// resize takes the CLI to a bounded size, and leaves the screen to take it
// once it has drawn what was printed before, at the size before. It never
// waits for the screen. Once the CLI is gone there is nothing to resize, and
// false says no answer is owed.
func (s *Session) resize(ask holdwire.SizeMsg) (holdwire.SizeMsg, bool) {
	cols, rows := holdwire.BoundSize(ask.Cols, ask.Rows)
	select {
	case <-s.exited:
		return holdwire.SizeMsg{}, false
	default:
	}
	s.sizing.Lock()
	defer s.sizing.Unlock()
	if err := s.child.Resize(cols, rows); err != nil {
		s.log("resizing the CLI failed", "err", err)
		return holdwire.SizeMsg{}, false
	}
	s.mu.Lock()
	z := sized{end: s.ground, cols: cols, rows: rows}
	// Two sizes at one offset are the last of them, and so is a size past
	// as many as the answers the screen may owe.
	if n := len(s.sizes); n > 0 && (s.sizes[n-1].end == z.end || n >= maxDue) {
		s.sizes[n-1] = z
	} else {
		s.sizes = append(s.sizes, z)
	}
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	nudge(s.emWake)
	return holdwire.SizeMsg{Cols: cols, Rows: rows}, true
}
