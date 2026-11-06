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
// each cursor query to answer it with the cursor as of that byte. The caller
// holds s.em.mu.
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
	s.noticeLoss()
	s.em.cols, s.em.rows = cols, rows
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return holdwire.SizeMsg{Cols: cols, Rows: rows}, true
}
