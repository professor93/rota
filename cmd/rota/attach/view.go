package attach

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/professor93/rota/cmd/rota/hold"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/vtscan"
)

// cropEvery is how often, at most, a crop is drawn: the attach calls flush no
// more often than this.
var cropEvery = 30 * time.Millisecond

// viewPendMax bounds what output keeps back from the screen while a sequence
// is open. A string that never ends is handed over in pieces rather than
// kept whole.
const viewPendMax = 64 << 10

// viewEmpty is a cropped row with nothing on it, as hold.Screen.Crop gives
// it.
const viewEmpty = "\x1b[0m\x1b[0m"

// view is what the person sees of a session: its bytes as they came when the
// window holds the session, and otherwise a crop of it, drawn from a copy of
// its screen kept here at the session's size.
//
// The copy is fed every byte of output, whichever the mode, so the view can
// change mode, repaint after an overlay, a switch or a reconnect, and draw a
// crop, with nothing asked of the server. A session too large for a screen,
// or whose screen panicked, has none: the view then passes bytes through
// when the window holds the session, and otherwise shows the note alone,
// never the session's bytes wrapped.
//
// It is not safe for concurrent use; the attach drives it from one
// goroutine.
type view struct {
	out  io.Writer
	note func(sessCols, sessRows, winCols, winRows int) string

	sessCols, sessRows int
	winCols, winRows   int

	scan   *vtscan.Scanner
	screen *hold.Screen // nil once there is none
	// pend is the output after the last ground point, kept from the screen
	// until the sequence it is in ends.
	pend []byte

	crop bool
	// modes is what the window was last given while cropped, and moded
	// says it was given any.
	modes vtscan.Modes
	moded bool
	// drawn is each row as last drawn, nil when the window has just been
	// cleared and every row must be drawn again.
	drawn [][]byte
	clear bool
	dirty bool
	x     int
}

// newView shows a session of sessCols by sessRows in a window of winCols by
// winRows. note gives the words drawn on the crop's last row, for the sizes
// as they are then.
func newView(out io.Writer, sessCols, sessRows, winCols, winRows int, note func(sessCols, sessRows, winCols, winRows int) string) *view {
	v := &view{
		out:      out,
		note:     note,
		sessCols: max(sessCols, 1), sessRows: max(sessRows, 1),
		winCols: max(winCols, 1), winRows: max(winRows, 1),
		scan: vtscan.NewScanner(),
	}
	if viewFits(v.sessCols, v.sessRows) {
		v.screen = hold.NewScreen(v.sessCols, v.sessRows)
		if !v.screen.OK() {
			v.lose()
		}
	}
	v.choose()
	return v
}

// viewFits says whether a screen is kept for a session of this size.
func viewFits(cols, rows int) bool {
	if cols > holdwire.MaxCols || rows > holdwire.MaxRows {
		return false
	}
	return holdwire.ScreenFits(uint16(cols), uint16(rows))
}

// output takes the session's next bytes.
func (v *view) output(p []byte) {
	if len(p) == 0 {
		return
	}
	res := v.scan.Feed(p)
	if v.screen != nil {
		// As the holder feeds its own screen: whole sequences, cut at the
		// last ground point, with one budget of repeats for the call.
		v.screen.Refill()
		if res.Ground >= 0 {
			v.screen.Write(v.pend)
			v.screen.Write(p[:res.Ground])
			v.pend = append(v.pend[:0], p[res.Ground:]...)
		} else {
			v.pend = append(v.pend, p...)
		}
		if len(v.pend) > viewPendMax {
			v.screen.Write(v.pend)
			v.pend = v.pend[:0]
		}
		if !v.screen.OK() {
			v.lose()
		}
	}
	if !v.crop {
		v.write(p)
		return
	}
	// With no screen there is nothing to draw but the note, and the
	// session's modes, which its keys need.
	if v.screen != nil || !v.moded || v.cropModes() != v.modes {
		v.dirty = true
	}
}

// cropModes are the session's modes as a crop gives them to the window:
// less the mouse, whose places would point at the wrong cells, and less the
// alternate screen, which Set leaves alone.
func (v *view) cropModes() vtscan.Modes {
	m := v.scan.Modes()
	m.Mouse, m.MouseEncoding, m.AltScreen = 0, 0, false
	return m
}

// resized follows the session to a new size: its screen with it, or none
// once a screen is no longer kept for it.
func (v *view) resized(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == v.sessCols && rows == v.sessRows {
		return
	}
	v.sessCols, v.sessRows = cols, rows
	if v.screen != nil {
		// What came before the new size is drawn at the old.
		v.screen.Write(v.pend)
		v.pend = v.pend[:0]
		if viewFits(cols, rows) {
			v.screen.Resize(cols, rows)
		} else {
			v.screen.Close()
		}
		if !v.screen.OK() {
			v.lose()
		}
	}
	v.resize()
}

// window follows the person's window to a new size.
func (v *view) window(cols, rows int) {
	cols, rows = max(cols, 1), max(rows, 1)
	if cols == v.winCols && rows == v.winRows {
		return
	}
	v.winCols, v.winRows = cols, rows
	v.resize()
}

// resize chooses the mode again after either size changed, and draws a crop
// that stays one whole, since the window's lines have moved.
func (v *view) resize() {
	if was := v.crop; !v.choose() && was {
		v.wipe()
		v.flush()
	}
}

// choose enters or leaves the crop as the sizes now say, and says whether
// it did either.
func (v *view) choose() bool {
	crop := v.winCols < v.sessCols || v.winRows < v.sessRows
	switch {
	case crop && !v.crop:
		v.crop = true
		// The crop draws on the window's own alternate screen, which the
		// next draw clears, with the session's modes less the mouse.
		v.write([]byte("\x1b[?1049h"))
		v.moded = false
		v.wipe()
		v.flush()
		return true
	case !crop && v.crop:
		v.crop = false
		v.drawn = nil
		// What the crop set on the alternate screen is cleared there, the
		// kitty flags among it, which are each screen's own; the repaint
		// then sets the session's modes on the screen it is on.
		v.write(append(vtscan.ResetModes(), "\x1b[?1049l"...))
		if !v.repaint() {
			// No screen to show: the session's modes at least, so its
			// keys still work.
			m := v.scan.Modes()
			b := vtscan.ResetModes()
			if m.AltScreen {
				b = append(b, "\x1b[?1049h\x1b[=0;1u"...)
			}
			v.write(append(b, m.Set()...))
		}
		return true
	}
	return false
}

// wipe has the next draw clear the window and draw every row.
func (v *view) wipe() {
	v.clear, v.drawn, v.dirty = true, nil, true
}

// redraw draws the session again whole: after an overlay, a switch or a
// reconnect. Passing through, it is the screen's repaint, or nothing when
// there is no screen; cropped, it is the whole crop.
func (v *view) redraw() {
	if !v.crop {
		v.repaint()
		return
	}
	v.moded = false
	v.wipe()
	v.flush()
}

// repaint writes the screen's repaint, and says whether there was one.
func (v *view) repaint() bool {
	if v.screen == nil {
		return false
	}
	b, ok := v.screen.Repaint(v.scan.Modes())
	if !v.screen.OK() {
		v.lose()
	}
	if ok {
		v.write(b)
	}
	return ok
}

// flush draws what of the crop changed since it was last drawn, and says
// whether it drew anything. Passing through, there is never anything to
// draw.
func (v *view) flush() bool {
	if !v.crop || !v.dirty {
		return false
	}
	var b bytes.Buffer
	// Autowrap is off while the crop draws, so that a character the
	// person's terminal counts wider than the screen does cannot wrap a row
	// and scroll the window, and back on before the write ends.
	b.WriteString("\x1b[?7l")
	m := v.cropModes()
	if !v.moded || m != v.modes {
		b.Write(vtscan.ResetModes())
		b.Write(m.Set())
		v.modes, v.moded = m, true
	}
	shown := v.winRows - 1
	var w hold.Window
	drew := false
	if v.screen != nil && shown > 0 {
		row, col := v.screen.Cursor()
		x, y := viewport(v.sessCols, v.sessRows, v.winCols, v.winRows, row-1, col-1, v.x)
		var ok bool
		if w, ok = v.screen.Crop(x, y, v.winCols, shown); ok {
			v.x, drew = x, true
		} else {
			v.lose()
		}
	}
	if v.clear {
		b.WriteString("\x1b[0m\x1b[H\x1b[2J")
		v.clear, v.drawn = false, nil
	}
	if drew {
		for r, line := range w.Rows {
			if v.drawn == nil && string(line) == viewEmpty {
				continue
			}
			if v.drawn != nil && r < len(v.drawn) && bytes.Equal(v.drawn[r], line) {
				continue
			}
			viewLine(&b, r+1, line, v.winCols)
		}
		v.drawn = w.Rows
	}
	b.WriteString("\x1b[" + strconv.Itoa(v.winRows) + ";1H\x1b[7m")
	text := viewNoteText(v.note(v.sessCols, v.sessRows, v.winCols, v.winRows), v.winCols)
	viewLine(&b, 0, []byte(text), v.winCols)
	b.WriteString("\x1b[?7h")
	if drew && w.CursorIn && !m.CursorHidden {
		b.WriteString("\x1b[" + strconv.Itoa(w.Row+1) + ";" + strconv.Itoa(w.Col+1) + "H\x1b[?25h")
	} else {
		b.WriteString("\x1b[?25l")
	}
	v.dirty = false
	v.write(b.Bytes())
	return true
}

// viewLine draws a row the window is wide enough for, at row r from 1 when r
// is not 0, and clears the rest of the line. A row as wide as the window is
// not followed by the clear, which would take the last cell with it: the
// cursor stays on that cell once it is written.
func viewLine(b *bytes.Buffer, r int, line []byte, cols int) {
	if r > 0 {
		b.WriteString("\x1b[" + strconv.Itoa(r) + ";1H")
	}
	b.Write(line)
	b.WriteString("\x1b[0m")
	if ansi.StringWidth(string(line)) < cols {
		b.WriteString("\x1b[K")
	}
}

// viewNoteText is the note with no control in it, cut to the window's width.
func viewNoteText(s string, cols int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	return ansi.Truncate(s, cols, "")
}

// cropped says whether the view is drawing a crop.
func (v *view) cropped() bool { return v.crop }

// screenOK says whether the view has a screen of the session.
func (v *view) screenOK() bool { return v.screen != nil && v.screen.OK() }

// lose lets go of a screen that has panicked or been closed. A crop then
// shows the note alone, on a window cleared of what the screen drew.
func (v *view) lose() {
	if v.screen == nil {
		return
	}
	v.screen.Close()
	v.screen, v.pend = nil, nil
	if v.crop {
		v.wipe()
	}
}

// close lets go of the screen. It writes nothing: putting the window back
// is the attach's.
func (v *view) close() {
	if v.screen != nil {
		v.screen.Close()
		v.screen = nil
	}
}

// write hands the window what the view draws. A failed write is the
// writer's to report: the attach ends on it.
func (v *view) write(p []byte) {
	if len(p) > 0 {
		_, _ = v.out.Write(p)
	}
}
