package hold

import (
	"strconv"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/ansi/parser"
)

// What x/vt v0.1.0 is not handed as the program wrote it. The program's bytes
// themselves are never changed, since viewers get them from the ring: only
// the emulator sees the difference, and with it draws what xterm draws.

// maxHeld bounds what Write keeps back from the emulator. A run of printed
// characters longer than this with no ASCII and no control in it is handed
// over as it is.
const maxHeld = 4096

// Write feeds the emulator what the program printed. It takes whole
// sequences: its caller cuts at vtscan's ground points, so the emulator never
// holds half a sequence when somebody asks for a repaint. Once the emulator
// has panicked, or the screen is closed, it does nothing.
//
// Two things are mended on the way:
//
//   - A control sequence x/vt would panic on, or spend seconds on, reaches it
//     with safe numbers; see csi. Repeats are bounded per write; see reps.
//   - The characters at the end of p are kept back until more arrives, or
//     until somebody reads the screen. x/vt finishes a grapheme cluster at
//     the end of every write, so a combining mark, a ZWJ or the second half
//     of a flag arriving in the next read would be dropped or drawn apart.
//     Everything before the last ASCII character or control is safe: x/vt
//     ends a cluster there on its own.
func (s *Screen) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return
	}
	defer s.catch()
	s.reps.budget = 2 * s.em.Width() * s.em.Height()
	for len(p) > 0 {
		at, next, tail := s.tame(p)
		if at < 0 {
			if tail >= 0 {
				s.hand(p[:tail])
				p = p[tail:]
			}
			s.held = append(s.held, p...)
			if len(s.held) > maxHeld {
				s.settle()
			}
			return
		}
		// At a fix, the emulator gets all of the sequence but its last
		// byte, CAN ends it there unacted on, and the safe one follows. At
		// the end of a run of repeats, what the run still owes goes first.
		s.hand(p[:at])
		_, _ = vtWrite(s.em, s.fix)
		s.fix = nil
		p = p[next:]
	}
	// When p ends in a repeat, the run pays what it owes now.
	s.pay()
}

// tame reads p up to the first place the emulator must be handed something
// p does not say, and says where that is: at is where the emulator's bytes
// from p stop, s.fix is what it gets there, and next is where p goes on. At
// the last byte of a control sequence that must not reach x/vt as it is, next
// is the byte after it. Before what ends a run of repeats that still owes the
// emulator some, next is at. With nothing to hand, at is -1, having read all
// of p.
//
// It also says where the run of printed characters at the end of p begins,
// as far as x/vt may yet join it to what follows: at its last ASCII
// character, at the first character after a control or sequence, len(p) when
// p ends in neither, and -1 when the run began before p, which is when p has
// no ASCII character, control or sequence at all.
func (s *Screen) tame(p []byte) (at, next, tail int) {
	tail = -1
	for i, b := range p {
		before := s.seq.State()
		if s.reps.run > 0 && before == parser.GroundState && !leadsWithREP(p[i:]) {
			// Whatever this is, it is not the next of a run of repeats.
			s.reps.run = 0
			if s.reps.owe > 0 {
				s.fix = s.owed()
				return i, i, tail
			}
		}
		act := s.seq.Advance(b)
		after := s.seq.State()
		switch {
		case s.fix != nil:
			return i, i + 1, tail
		case before == parser.Utf8State || after == parser.Utf8State:
			// A byte of a character that is not ASCII.
		case before == parser.GroundState && act == parser.PrintAction:
			tail = i
		case before == parser.GroundState && act == parser.NoneAction:
			// A byte x/vt ignores, and the cluster with it.
		default:
			tail = i + 1
		}
	}
	return -1, -1, tail
}

// hand gives the emulator what is held, then p, in one write, so a cluster
// begun in the one is finished in the other.
func (s *Screen) hand(p []byte) {
	if len(s.held) > 0 {
		p = append(s.held, p...)
		s.held = s.held[:0]
	}
	if len(p) > 0 {
		_, _ = vtWrite(s.em, p)
	}
}

// settle hands the emulator what is held, for a reader that must see all the
// program has printed.
func (s *Screen) settle() { s.hand(nil) }

// csi is the parser's CSI handler, and decides which sequences x/vt v0.1.0
// cannot take as they are:
//
//   - Margins past the edge of the screen. DECSTBM (CSI t;b r) and DECSLRM
//     (CSI l;r s) keep a bottom or right margin larger than the screen, and
//     the next scroll, insert or delete indexes past the end of a row and
//     panics. xterm takes such a margin as the edge of the screen, and so
//     does the emulator after the fix.
//   - Counts larger than the screen. REP (CSI n b) prints n characters one by
//     one, about a minute for n = 99999999; ECH, CHT and CBT (CSI n X, I, Z)
//     loop n times, up to a second. Past the screen's width, erasing or
//     tabbing more changes nothing. Repeats are repeat's to decide.
//
// A fix is CAN, which ends the sequence the emulator is reading without acting
// on it, then the sequence with the safe numbers, or nothing.
func (s *Screen) csi(cmd ansi.Cmd, params ansi.Params) {
	w, h := s.em.Width(), s.em.Height()
	// The emulator reads each of these parameters with the same defaults.
	first, _, _ := params.Param(0, 1)
	switch cmd {
	case 'r':
		if bottom, _, _ := params.Param(1, h); bottom > h {
			s.fix = fixed(cmd, first, h)
		}
	case 's':
		if right, _, _ := params.Param(1, w); right > w {
			s.fix = fixed(cmd, first, w)
		}
	case 'b':
		s.fix = s.repeat(first, w, h)
	case 'X', 'I', 'Z':
		if first > w {
			s.fix = fixed(cmd, w)
		}
	}
}

// reps is what Write keeps about REP, so that no write of any length holds
// the screen for longer than two screens of repeats take to draw.
type reps struct {
	// budget is how many repeats past the first row's worth of each REP
	// this write may still draw. A REP of a row or less costs what printing
	// a row costs, as any output may; what a REP repeats past a row is drawn
	// only to be drawn over, and a write packed with such REPs held the
	// screen for seconds.
	budget int
	// run is how many characters the emulator has drawn for the REPs read
	// one straight after another since the last of anything else, up to two
	// screens. A run of REPs is one REP of their total count, so once it has
	// drawn two screens, what each further one adds is its count modulo the
	// width, kept in owe.
	run int
	// owe is what the run still owes the emulator, fewer than a row. It is
	// paid before whatever ends the run, or at the end of the write, so a
	// reader never sees the screen without it.
	owe int
}

// repeat decides what the emulator gets for a REP of n on a w×h screen: nil
// for the REP as it is, or a fix. A REP is drawn whole or, past the budget,
// not at all. That is the one place tame gives up drawing what x/vt would,
// to stay quick, and only for a write that repeats more than two screens
// past a row's worth.
func (s *Screen) repeat(n, w, h int) []byte {
	r := &s.reps
	left := 2*w*h - r.run
	if left <= 0 {
		r.owe = (r.owe + n%w) % w
		return []byte{can}
	}
	c := repeats(n, left, w)
	if past := c - w; past > 0 {
		if past > r.budget {
			return []byte{can}
		}
		r.budget -= past
	}
	r.run += min(c, left)
	if c == n {
		return nil
	}
	return fixed('b', c)
}

// owed is the REP that pays what the run owes, and says it is paid.
func (s *Screen) owed() []byte {
	b := fixed('b', s.reps.owe)[1:]
	s.reps.owe = 0
	return b
}

// pay hands the emulator what the run owes.
func (s *Screen) pay() {
	if s.reps.owe > 0 {
		_, _ = vtWrite(s.em, s.owed())
	}
}

// leadsWithREP says p begins with a whole REP and nothing in it but its
// count: CSI, digits, b. Only such a REP goes on a run of them.
func leadsWithREP(p []byte) bool {
	if len(p) < 3 || p[0] != 0x1b || p[1] != '[' {
		return false
	}
	for _, c := range p[2:] {
		switch {
		case c == 'b':
			return true
		case c < '0' || c > '9':
			return false
		}
	}
	return false
}

// repeats is how many times REP repeats a character on a screen w wide to
// draw what n times draws, for a run of REPs left short of two screens of
// repeats. Past 2wh, n and n-w draw the same: by wh the cursor has reached
// the row where every wrap scrolls, or the last row, where it stays, and by
// another wh every row that can scroll has scrolled through, so all that is
// left is the column n ends in. A run is one REP of its total count, so what
// it has drawn counts towards the 2wh.
func repeats(n, left, w int) int {
	if n >= left+w {
		return left + (n-left)%w
	}
	return n
}

// can ends the sequence the emulator is reading, unacted on.
const can = 0x18

// fixed is CAN, then CSI with the given parameters and final byte.
func fixed(final ansi.Cmd, params ...int) []byte {
	b := []byte{can, 0x1b, '['}
	for i, p := range params {
		if i > 0 {
			b = append(b, ';')
		}
		b = strconv.AppendInt(b, int64(p), 10)
	}
	return append(b, byte(final))
}
