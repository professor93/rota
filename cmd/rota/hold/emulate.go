//go:build linux || darwin

package hold

import (
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/internal/vtscan"
)

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
// each cursor query to answer it with the cursor as of that byte, and at each
// size the terminal took to take it there. The pieces are one batch, and
// share one budget of repeats. The caller holds s.em.mu.
//
// A screen the ring has overtaken cannot know what it missed, so it is
// retired for the session's life, said once, and the cursor is the corner
// from then on, as it is for a screen whose emulator panicked.
func (s *Session) catchUp() {
	defer s.noticeLoss()
	s.mu.Lock()
	target, modes := s.ground, s.gmodes
	n := 0
	for n < len(s.dues) && s.dues[n].end <= target {
		n++
	}
	dues := make([]due, n)
	copy(dues, s.dues)
	s.dues = append(s.dues[:0], s.dues[n:]...)
	k := 0
	for k < len(s.sizes) && s.sizes[k].end <= target {
		k++
	}
	sizes := make([]sized, k)
	copy(sizes, s.sizes)
	s.sizes = append(s.sizes[:0], s.sizes[k:]...)
	s.owed += n
	defer func() {
		s.mu.Lock()
		s.owed -= n
		s.drawn = s.em.off
		s.mu.Unlock()
	}()
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
			s.sized(sizes[0].cols, sizes[0].rows)
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
}

// tooLarge is what the log says of a terminal larger than a screen is kept
// for.
const tooLarge = "the terminal is larger than a screen is kept for; viewers get the ring replayed"

// sized takes the screen to the size the terminal took, or retires it for a
// size larger than a screen is kept for. The caller holds s.em.mu.
func (s *Session) sized(cols, rows uint16) {
	s.em.cols, s.em.rows = cols, rows
	if s.em.retired {
		return
	}
	if !holdwire.ScreenFits(cols, rows) {
		s.em.retired = true
		s.screen.Close()
		s.log(tooLarge, "cols", cols, "rows", rows, "cells", holdwire.MaxScreenCells)
		return
	}
	s.screen.Resize(int(cols), int(rows))
}

// noticeLoss retires the screen for the session's life once its emulator
// has panicked, and says so in one line with what it panicked with: the
// Screen keeps the holder up, and the log says why viewers get the ring
// replayed. The caller holds s.em.mu.
func (s *Session) noticeLoss() {
	if s.em.retired {
		return
	}
	if why := s.screen.Lost(); why != "" {
		s.em.retired = true
		s.log("the screen's emulator panicked and is retired; viewers get the ring replayed", "panic", why)
	}
}

// putScreen makes a Screen payload. It is a variable so a test can make one
// the daemon could not take.
var putScreen = holdwire.PutScreen

// maxBehind is how far the screen may be behind the CLI for a repaint to
// wait for it: one read of output. Further behind, catching up could take
// seconds, and the daemon has replayed the ring long before.
const maxBehind = pumpRead

// behind says whether the screen is further behind the CLI than a repaint
// waits for, as of its last catch-up.
func (s *Session) behind() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ground-s.drawn > maxBehind
}

// repaint is a Screen payload: the screen once it has caught up, the offset
// it stands for and the modes on there. OK is false with no emulator, a
// screen too far behind to wait for, or a repaint too big for a frame, and
// then it carries no repaint: the daemon replays the ring instead. Every
// ScreenAsk gets an answer it can take.
//
// A screen far behind is answered at once, without waiting for the emulator
// to finish what it is drawing: a flood at a large size can hold it for
// minutes.
func (s *Session) repaint(seq uint64) []byte {
	if !s.em.mu.TryLock() {
		if s.behind() {
			return s.unpainted(seq)
		}
		s.em.mu.Lock()
	}
	defer s.em.mu.Unlock()
	if s.behind() {
		return s.unpainted(seq)
	}
	s.catchUp()
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
