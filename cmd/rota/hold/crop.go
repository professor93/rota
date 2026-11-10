package hold

import (
	"bytes"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// Window is part of a screen, cut out for a terminal smaller than it.
//
// Rows holds one entry for each row of the window: that screen row's styled
// cells from the window's first column, as many as the window is wide. Each
// starts from ESC[0m and ends with ESC[0m, and has no cursor movement, no
// erase and no trailing blanks, so it can be drawn at the start of any line
// or put in a box. Row and Col are where the cursor is, counted from the
// window's top left corner and from 0, and CursorIn says whether that is
// inside the window.
type Window struct {
	Rows     [][]byte
	Row, Col int
	CursorIn bool
}

// cropEmpty is a row with nothing on it.
const cropEmpty = "\x1b[0m\x1b[0m"

// Crop cuts out the window of cols by rows cells whose top left corner is at
// column x and row y of the screen, both from 0. Cells past the screen are
// blank, and a wide character either edge cuts is a space, so no row is ever
// wider than cols. Only the visible screen is cut; scrollback is not.
//
// It says false, with nothing, when there is no emulator.
func (s *Screen) Crop(x, y, cols, rows int) (w Window, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return Window{}, false
	}
	// A panic leaves nothing, as it leaves no screen.
	defer func() {
		if !ok {
			w = Window{}
		}
	}()
	defer s.catch()
	s.settle()
	em := s.em
	cols, rows = max(cols, 0), max(rows, 0)
	w.Rows = make([][]byte, rows)
	var b bytes.Buffer
	for r := range rows {
		w.Rows[r] = cropRow(&b, em, x, y+r, cols)
	}
	p := em.CursorPosition()
	w.Row, w.Col = p.Y-y, p.X-x
	w.CursorIn = w.Row >= 0 && w.Row < rows && w.Col >= 0 && w.Col < cols
	return w, true
}

// cropCell is what one place in a cropped row shows: a cell's text and
// style, or a space for a cell that is not there or is cut.
type cropCell struct {
	text  string
	style uv.Style
	blank bool
}

// cropRow draws cols cells of row y from column x, into a slice of its own.
func cropRow(b *bytes.Buffer, em *vt.Emulator, x, y, cols int) []byte {
	if y < 0 || y >= em.Height() || cols == 0 {
		return []byte(cropEmpty)
	}
	width := em.Width()
	cells := make([]cropCell, 0, cols)
	space := func(st uv.Style) cropCell {
		return cropCell{text: " ", style: st, blank: st.IsZero()}
	}
	for i := 0; i < cols; i++ {
		cx := x + i
		if cx < 0 || cx >= width {
			cells = append(cells, space(uv.Style{}))
			continue
		}
		c := em.CellAt(cx, y)
		switch {
		case c == nil:
			cells = append(cells, space(uv.Style{}))
		case c.Width == 0:
			// The second half of a wide character is drawn by its first,
			// unless the left edge cut that off.
			if i == 0 {
				st := c.Style
				if cx > 0 {
					if lead := em.CellAt(cx-1, y); lead != nil {
						st = lead.Style
					}
				}
				cells = append(cells, space(st))
			}
		case i+c.Width > cols:
			// A wide character the right edge cuts.
			for ; i < cols; i++ {
				cells = append(cells, space(c.Style))
			}
		default:
			cl := cropCell{text: c.Content, style: c.Style, blank: blank(c)}
			if cl.text == "" {
				cl.text = " "
			}
			cells = append(cells, cl)
			i += c.Width - 1
		}
	}
	for len(cells) > 0 && cells[len(cells)-1].blank {
		cells = cells[:len(cells)-1]
	}
	b.Reset()
	b.WriteString("\x1b[0m")
	var pen uv.Style
	for _, c := range cells {
		if !c.style.Equal(&pen) {
			// The row starts from a reset, and a zero pen is one.
			if !pen.IsZero() {
				b.WriteString("\x1b[0m")
			}
			if !c.style.IsZero() {
				b.WriteString(c.style.String())
			}
			pen = c.style
		}
		b.WriteString(c.text)
	}
	b.WriteString("\x1b[0m")
	return bytes.Clone(b.Bytes())
}

// Size is the screen's size, or 0 by 0 when there is no emulator.
func (s *Screen) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ok {
		return 0, 0
	}
	defer s.catch()
	return s.em.Width(), s.em.Height()
}
