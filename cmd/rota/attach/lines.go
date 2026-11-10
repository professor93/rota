package attach

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Line is the one line an attach ends with: why it ended and, when the
// session goes on, how to come back to it. The session is named by its name,
// cleaned, or by its id when it has none, and how to come back is o.Again of
// Word of it, or `rota run --attach` and the word when o.Again is nil:
//
//	rota: detached from api; rota run --attach api returns to it
//	rota: api ended, exit 0
//	rota: api was interrupted: its holder went without its CLI's exit
//	rota: lost api: <why>; rota run --attach api tries again
//	rota: <why it was refused>
//
// A Result with no Why is said as Refused.
func (r Result) Line(o Options) string {
	name := OneLine(r.Session.Name)
	if name == "" {
		name = OneLine(r.Session.ID)
	}
	again := "rota run --attach " + Word(r.Session)
	if o.Again != nil {
		again = o.Again(Word(r.Session))
	}
	switch r.Why {
	case Detached:
		return "rota: detached from " + name + "; " + again + " returns to it"
	case Ended:
		if r.Code < 0 {
			return "rota: " + name + " was interrupted: its holder went without its CLI's exit"
		}
		return "rota: " + name + " ended, exit " + strconv.Itoa(r.Code)
	case Lost:
		if r.Err == nil {
			return "rota: lost " + name + "; " + again + " tries again"
		}
		return "rota: lost " + name + ": " + OneLine(r.Err.Error()) + "; " + again + " tries again"
	}
	if r.Err == nil {
		return "rota: the attach to " + name + " was refused"
	}
	return "rota: " + OneLine(r.Err.Error())
}

// linesKeep is the order in which a narrow hint row keeps its items, by
// their place in the row: leaving first, then help, which leads to the
// rest, then fitting, which a cropped window needs, then the others.
var linesKeep = []int{0, 4, 3, 1, 2, 5}

// hintRow is the one row drawn when no second key follows the chord key in
// time:
//
//	Ctrl-\  d leave · n/p next/prev · k keyboard · z fit · ? help · Ctrl-\ itself
//
// A window narrower than that drops whole items, so none is cut in half.
// It takes them in this order, each that still fits: d leave, ? help,
// z fit, n/p next/prev, k keyboard, the key itself. Those kept are written
// in the row's own order, so a 40-cell window reads
//
//	Ctrl-\  d leave · z fit · ? help
//
// A window with no room for d leave shows the key alone, and one too
// narrow for that gets as much of the key as fits. It is never wider than
// cols.
func hintRow(key byte, cols int) string {
	k := KeyName(key)
	items := []string{"d leave", "n/p next/prev", "k keyboard", "z fit", "? help", k + " itself"}
	if ansi.StringWidth(k) > cols {
		return ansi.Truncate(k, max(cols, 0), "")
	}
	kept := make([]bool, len(items))
	wide := ansi.StringWidth(k) - 1 // the first item's separator is two cells, not three
	for n, i := range linesKeep {
		w := wide + 3 + ansi.StringWidth(items[i])
		if w <= cols {
			kept[i], wide = true, w
		} else if n == 0 {
			break // with no room to say how to leave, the key is the row
		}
	}
	row := k
	sep := "  "
	for i, it := range items {
		if kept[i] {
			row += sep + it
			sep = " · "
		}
	}
	return row
}

// helpCard is the card that ? shows, one string per line, naming the keys
// that may follow the chord key. With teach it begins with the line shown
// before a person's first attach, saying how to leave.
func helpCard(key byte, teach bool) []string {
	k := KeyName(key)
	var card []string
	if teach {
		card = append(card, "rota: to leave, press "+k+" then d; the session goes on")
	}
	card = append(card, "rota: after "+k+", press")
	for _, e := range [][2]string{
		{"d", "leave; the session goes on"},
		{"n  p", "next, previous session"},
		{"k", "take the keyboard"},
		{"z", "fit the session to this window"},
		{"?", "these keys"},
		{k, "send " + k + " itself"},
	} {
		card = append(card, "  "+linesPad(e[0]+" ", 9)+e[1])
	}
	return card
}

// cropNote is the note a window smaller than its session is shown in its
// last row: first the key that fits the session here, so a phone's narrow
// row still says it, then how big each is.
//
//	rota: Ctrl-\ z fits; api is 100×30, this window 40×12
func cropNote(name string, key byte, sessCols, sessRows, winCols, winRows int) string {
	return fmt.Sprintf("rota: %s z fits; %s is %d×%d, this window %d×%d",
		KeyName(key), OneLine(name), sessCols, sessRows, winCols, winRows)
}

// drawRow draws text over row, counted from 1, of a window cols wide: the
// cursor saved, the row in reverse video holding text cleaned and cut or
// padded to exactly cols cells, the attributes reset and the cursor put
// back. What the session drew there is repainted from the attach's own copy
// of its screen once the row is no longer wanted.
func drawRow(row int, text string, cols int) []byte {
	var b strings.Builder
	b.WriteString("\x1b7")
	fmt.Fprintf(&b, "\x1b[%d;1H", row)
	b.WriteString("\x1b[7m")
	b.WriteString(linesFit(OneLine(text), cols))
	b.WriteString("\x1b[0m\x1b8")
	return []byte(b.String())
}

// drawCard draws lines as a card centred in a window cols by rows: a
// one-cell border, a space inside it either side, and each line cleaned and
// cut or padded to the card's width. A card that does not fit is cut, to
// the window's width and to as many of its first lines as there are rows,
// so it never writes past the window. The cursor is saved and put back, as
// drawRow does, with the attributes reset while the card is drawn. A window
// with no room for a border and one cell inside it draws nothing.
func drawCard(lines []string, cols, rows int) []byte {
	wide := 0
	for _, l := range lines {
		wide = max(wide, ansi.StringWidth(OneLine(l)))
	}
	w := min(wide+4, cols)
	h := min(len(lines)+2, rows)
	if w < 5 || h < 3 {
		return nil
	}
	top, left := (rows-h)/2+1, (cols-w)/2+1
	var b strings.Builder
	b.WriteString("\x1b7\x1b[0m")
	at := func(row int, s string) {
		fmt.Fprintf(&b, "\x1b[%d;%dH", top+row, left)
		b.WriteString(s)
	}
	at(0, "┌"+strings.Repeat("─", w-2)+"┐")
	for i, l := range lines[:h-2] {
		at(1+i, "│ "+linesFit(OneLine(l), w-4)+" │")
	}
	at(h-1, "└"+strings.Repeat("─", w-2)+"┘")
	b.WriteString("\x1b[0m\x1b8")
	return []byte(b.String())
}

// linesFit is s cut or padded with spaces to exactly w cells.
func linesFit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return linesPad(ansi.Truncate(s, w, ""), w)
}

// linesPad is s padded with spaces to at least w cells.
func linesPad(s string, w int) string {
	if n := w - ansi.StringWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
