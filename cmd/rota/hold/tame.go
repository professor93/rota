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
//     with safe numbers; see csi.
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
	for len(p) > 0 {
		fix, tail := s.tame(p)
		if fix >= 0 {
			// The emulator gets all of the sequence but its last byte. CAN
			// ends it there, unacted on, and the safe one follows.
			s.hand(p[:fix])
			_, _ = vtWrite(s.em, s.fix)
			s.fix = nil
			p = p[fix+1:]
			continue
		}
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
}

// tame reads p up to the last byte of the first control sequence that must
// not reach x/vt as it is, and says where that byte is, with s.fix set to
// what the emulator gets in its place, or -1, having read all of p.
//
// It also says where the run of printed characters at the end of p begins,
// as far as x/vt may yet join it to what follows: at its last ASCII
// character, at the first character after a control or sequence, len(p) when
// p ends in neither, and -1 when the run began before p, which is when p has
// no ASCII character, control or sequence at all.
func (s *Screen) tame(p []byte) (fix, tail int) {
	tail = -1
	for i, b := range p {
		before := s.seq.State()
		act := s.seq.Advance(b)
		after := s.seq.State()
		switch {
		case s.fix != nil:
			return i, tail
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
	return -1, tail
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
//     tabbing more changes nothing. Past two screens of repeats, the screen
//     is filled and the only thing a longer run changes is how far into its
//     last row it ends, which depends on n modulo the width, so the count
//     keeps that and drops whole rows.
//
// A fix is CAN, which ends the sequence the emulator is reading without acting
// on it, then the sequence with the safe numbers.
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
		if n := repeats(first, w, h); n != first {
			s.fix = fixed(cmd, n)
		}
	case 'X', 'I', 'Z':
		if first > w {
			s.fix = fixed(cmd, w)
		}
	}
}

// repeats is how many times REP repeats a character on a w×h screen to draw
// what n times draws. Past 2wh, n and n-w draw the same: by wh the cursor has
// reached the row where every wrap scrolls, or the last row, where it stays,
// and by another wh every row that can scroll has scrolled through, so all
// that is left is the column n ends in.
func repeats(n, w, h int) int {
	if floor := 2 * w * h; n >= floor+w {
		return floor + n%w
	}
	return n
}

// fixed is CAN, then CSI with the given parameters and final byte.
func fixed(final ansi.Cmd, params ...int) []byte {
	b := []byte{0x18, 0x1b, '['}
	for i, p := range params {
		if i > 0 {
			b = append(b, ';')
		}
		b = strconv.AppendInt(b, int64(p), 10)
	}
	return append(b, byte(final))
}
