// Package hold is the holder: the process that keeps one session's terminal
// alive, whoever is or is not looking at it.
package hold

import (
	"bytes"
	"io"
	"strconv"
	"sync"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/internal/share"
	"github.com/professor93/rota/internal/vtscan"
)

// maxRepaint bounds a repaint, so that it fits in one frame with room for
// what travels beside it. A bigger screen is replayed from the ring instead.
const maxRepaint = share.MaxFrame - 4096

// vtWrite is how a Screen hands bytes to the emulator. It is a variable so a
// test can make the emulator panic and see that nothing else does.
var vtWrite = (*vt.Emulator).Write

// Screen is what a held session's terminal shows right now, kept by x/vt.
//
// x/vt is young, and whatever it does with a byte it did not expect must not
// take the holder down with it: a session outlives its viewers, its daemon
// and its emulator. Every call into the emulator runs under one mutex and
// behind recover(). The first panic retires the emulator for good, OK says
// so from then on, and the holder falls back to replaying its ring, which is
// what a viewer got before there was a screen at all.
//
// recover() is the backstop, not the plan. What x/vt v0.1.0 is known to fall
// over on, spend seconds on or draw wrong when it comes in pieces is mended
// on the way in, in tame.go.
type Screen struct {
	mu sync.Mutex
	em *vt.Emulator
	ok bool

	// seq is x/vt's own parser, x/ansi's, fed every byte just ahead of the
	// emulator, so it is in the emulator's state at every byte. Its CSI
	// handler sets fix when the sequence just read must not reach x/vt as
	// it is.
	seq *ansi.Parser
	fix []byte
	// held is the end of what the program has printed so far, kept from
	// the emulator until what follows shows whether it is finished.
	held []byte
	reps reps
}

// NewScreen starts an emulator of the given size.
//
// The emulator answers a program's queries by writing into a pipe, and an
// unread pipe stops its Write until somebody reads. The holder answers those
// queries itself, from vtscan, so a goroutine reads the pipe and throws the
// emulator's answers away until Close.
func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	func() {
		defer s.catch()
		s.em = vt.NewEmulator(cols, rows)
		s.ok = true
	}()
	if s.ok {
		go drain(s.em)
	}
	// The emulator's parser has room for as many parameters, which is what
	// decides the values a CSI handler sees. It has room for 4 MiB of string
	// data too, which changes nothing about where a sequence ends, so this
	// one keeps none.
	s.seq = ansi.NewParser()
	s.seq.SetParamsSize(maxParams)
	s.seq.SetDataSize(1)
	s.seq.SetHandler(ansi.Handler{HandleCsi: s.csi})
	return s
}

// maxParams is how many parameters x/vt's parser keeps (x/ansi's
// parser.MaxParamsSize, which NewEmulator sets).
const maxParams = 32

// drain reads and discards the emulator's answers until the pipe closes.
//
// Emulator.Close is never called: it writes a field Emulator.Read looks at
// without a lock, and drain is in Read at any moment. Closing the pipe's
// writer instead ends the read with EOF and touches nothing Read looks at.
func drain(em *vt.Emulator) {
	buf := make([]byte, 4096)
	for {
		if _, err := em.Read(buf); err != nil {
			return
		}
	}
}

// catch turns a panic inside the emulator into a retired screen. It is
// deferred by every method that calls into the emulator, under the mutex.
func (s *Screen) catch() {
	if recover() != nil {
		s.retire()
	}
}

// retire gives up the emulator. It closes the answer pipe's writer, so drain
// ends, and never calls into the emulator again.
func (s *Screen) retire() {
	s.ok = false
	if s.em == nil {
		return
	}
	if c, ok := s.em.InputPipe().(io.Closer); ok {
		func() {
			defer func() { _ = recover() }()
			_ = c.Close()
		}()
	}
}

// Resize follows the terminal to a new size.
func (s *Screen) Resize(cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok || cols < 1 || rows < 1 {
		return
	}
	defer s.catch()
	s.settle()
	s.em.Resize(cols, rows)
	// What a run of repeats drew is not a full screen at the new size.
	s.reps.run = 0
}

// Cursor is where the cursor is, 1-based, as a CPR reports it. Without an
// emulator it is the top left corner.
func (s *Screen) Cursor() (row, col int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, col = 1, 1
	if !s.ok {
		return
	}
	defer s.catch()
	s.settle()
	p := s.em.CursorPosition()
	return p.Y + 1, p.X + 1
}

// Repaint draws the screen as it is now, for a terminal in any state: a
// viewer that has just arrived, or one whose bytes fell behind. It leaves
// every mode rota tracks as m says, the screen m.AltScreen names showing
// what the emulator holds, and the cursor where the emulator has it. Only the
// visible screen is drawn; scrollback is not.
//
// It says false, with nothing, when there is no emulator or the repaint is
// longer than maxRepaint.
func (s *Screen) Repaint(m vtscan.Modes) (out []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return nil, false
	}
	// A panic leaves the results as they start, nil and false.
	defer s.catch()
	s.settle()
	em := s.em
	var b bytes.Buffer
	// Every tracked mode off and the cursor shown on the screen the viewer
	// is on, then the session's screen, whose kitty flags are its own and
	// are cleared too: m.Set writes nothing for none.
	b.Write(vtscan.ResetModes())
	if m.AltScreen {
		b.WriteString("\x1b[?1049h")
	} else {
		b.WriteString("\x1b[?1049l")
	}
	b.WriteString("\x1b[=0;1u\x1b[0m\x1b[H\x1b[2J")
	for y := range em.Height() {
		row(&b, em, y)
		if b.Len() > maxRepaint {
			return nil, false
		}
	}
	b.Write(m.Set())
	p := em.CursorPosition()
	cup(&b, p.Y, p.X)
	if m.CursorHidden {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	if b.Len() > maxRepaint {
		return nil, false
	}
	return b.Bytes(), true
}

// row draws one row of the emulator's screen onto a cleared one: its cells
// up to the last that shows anything, each style set from a reset, and the
// style reset at the end.
func row(b *bytes.Buffer, em *vt.Emulator, y int) {
	last := -1
	for x := em.Width() - 1; x >= 0; x-- {
		if c := em.CellAt(x, y); c != nil && c.Width > 0 && !blank(c) {
			last = x
			break
		}
	}
	cup(b, y, 0)
	col := 0
	var pen uv.Style
	for x := 0; x <= last; x++ {
		c := em.CellAt(x, y)
		// The second half of a wide character is drawn by its first.
		if c == nil || c.Width == 0 {
			continue
		}
		if x != col {
			cup(b, y, x)
			col = x
		}
		if !c.Style.Equal(&pen) {
			b.WriteString("\x1b[0m")
			if !c.Style.IsZero() {
				b.WriteString(c.Style.String())
			}
			pen = c.Style
		}
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
		col += c.Width
	}
	b.WriteString("\x1b[0m")
}

// blank is a cell a cleared screen already shows.
func blank(c *uv.Cell) bool {
	return (c.Content == " " || c.Content == "") && c.Style.IsZero() && c.Link.IsZero()
}

// cup moves the cursor to a 0-based row and column.
func cup(b *bytes.Buffer, y, x int) {
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(x + 1))
	b.WriteByte('H')
}

// OK says the emulator is alive: it has not panicked and the screen is not
// closed.
func (s *Screen) OK() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ok
}

// Close retires the emulator and ends the goroutine reading its answers.
func (s *Screen) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retire()
}
