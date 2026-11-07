package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/professor93/rota/internal/pty"
)

// grid is a table drawn with a frame: a header, rows whose cells may span
// lines, and one space of padding on either side of a cell — the least that
// keeps a border from touching what it borders.
//
// Rows that span lines are ruled apart, because three lines of usage under
// one account and one under the next have no other way of saying where one
// account ends. One-line rows are not: a rule per row would double a short
// listing for nothing.
type grid struct {
	header []string
	rows   [][]string // a cell with newlines in it spans lines

	// width is the terminal's, when the table is drawn on one; 0 draws the
	// table at its natural width, whatever that is. A terminal narrower than
	// the table would otherwise wrap every long line itself, taking the
	// borders with it and breaking every box on the screen.
	width int
	// give is the columns of free text, which wrap their cells at their
	// spaces to fit a narrow terminal, in the order they give way, each
	// first only down to its room.
	give []wrap
	// drop is the short columns — an age, a threshold, a CLI's name — left
	// out whole, in this order, while the free text still reads: each once
	// every column in give is down to its room, or to the width it names.
	// Cut to fit, they would say "cl…" under a header cut to "C…".
	drop []leave
	// cut is the columns whose lines are cut, each keeping its start and an
	// "…", once the free text is as narrow as it reads. A column named
	// nowhere — a place, an id — keeps its width whatever happens.
	cut []int
	// cutBy is how a column is cut where cutCell's way would lose what it
	// is there for: a path keeps its end, the part no other path on the
	// screen shares (cutStart), and "#1 a@x" is cut where the width falls
	// (cut), since going back to its space leaves "#1…" and no account.
	cutBy map[int]func(c string, n int) string
	// steps, when set, is the order the columns are cut in, in place of the
	// one give, drop and cut make (see fit), for a table with no column to wrap
	// whose columns each give way only so far before the next is touched:
	// a column can be in more than one step, each with a floor of its own.
	steps []stage

	// plain draws the rows with no frame and no header, lined up the way
	// text/tabwriter lines them up: two spaces in, and each column but the
	// last two spaces wider than its widest cell. It is the sessions
	// listing's look, kept on a terminal wide enough for it, so fitting a
	// narrow one changes nothing anywhere else.
	plain bool
}

// wrap is a column that wraps, and how far it wraps before any short column
// is left out instead.
type wrap struct{ col, room int }

// leave is a short column left out to fit, and how narrow the free text
// wraps before it goes: 0 for no narrower than its room.
type leave struct{ col, after int }

// termWidth is how many columns wide out is, when it is a terminal, and 0
// when it is not — a pipe or a file gets what it always got, at whatever
// width that is, because nothing reading it has a width to fit.
//
// The terminal is asked first. COLUMNS is what a shell said once and a
// dragged window has made a liar of since, so it is only the answer for a
// terminal that will not say.
func termWidth(out io.Writer) int {
	f, ok := out.(*os.File)
	if !ok || !isTerminal(f) {
		return 0
	}
	if cols, _, err := pty.Size(f); err == nil && cols > 0 {
		return int(cols)
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return 0
}

// say prints text, each line of it wrapped at its spaces to width when
// width is a terminal's, a command in backticks kept whole (see wrapLine).
// Without one, it is printed as it is. Either way a control character in
// it, which an address, a reason or a provider's error may carry, is a
// space (see printable): a note is wrapped by its count of runes, and an
// escape would recolour or retitle the terminal it is said on.
func say(out io.Writer, width int, text string) {
	text = printable(text)
	if width > 0 {
		var lines []string
		for line := range strings.SplitSeq(text, "\n") {
			lines = append(lines, wrapLine(line, width)...)
		}
		text = strings.Join(lines, "\n")
	}
	io.WriteString(out, text)
}

// How narrow columns get once nothing is left to drop (see fit): the free
// text wraps to wrapFloor before anything is cut, and the first cut goes to
// cutSoft, "5h 100%…", a spent window keeping its percentage and "Fable 48%"
// cut to its name. cutHard, a character and an "…", is for grid.steps.
const (
	wrapFloor = 10
	cutSoft   = 8
	cutHard   = 2
)

// add puts a row under the table's header, each control character in its
// cells a space (see printable): a cell is measured by its runes, and a tab
// or an escape in an address, a status or a reason would move the cursor
// where no count can follow, or be taken by the terminal as a command
// rather than drawn. A newline is kept, since it is what parts a cell's
// lines.
func (g *grid) add(cells ...string) {
	row := make([]string, len(cells))
	for i, c := range cells {
		row[i] = printable(c)
	}
	g.rows = append(g.rows, row)
}

// widths is the widest line in each column, header included, in the
// columns it takes on screen (see screenWidth).
func widths(header []string, rows [][]string) []int {
	n := len(header)
	for _, r := range rows {
		n = max(n, len(r))
	}
	w := make([]int, n)
	measure := func(cells []string) {
		for i, c := range cells {
			for line := range strings.SplitSeq(c, "\n") {
				w[i] = max(w[i], screenWidth(line))
			}
		}
	}
	measure(header)
	for _, r := range rows {
		measure(r)
	}
	return w
}

// head is column i's header, "" in a table without one.
func (g *grid) head(i int) string {
	if i < len(g.header) {
		return g.header[i]
	}
	return ""
}

// frame is how wide a table with these column widths is drawn: each column
// is a border, a space, its content and a space, and one border closes it.
// A plain one is two spaces in, and each column but the last its content
// and the two spaces between it and the next.
func (g *grid) frame(w []int) int {
	if g.plain {
		n := 2
		for i, c := range w {
			n += c
			if i < len(w)-1 {
				n += 2
			}
		}
		return n
	}
	n := 1
	for _, c := range w {
		n += c + 3
	}
	return n
}

// fit is the table's cells as drawn: as they are when the table fits, or
// when no width was asked for, and otherwise narrowed until it does, in
// stages, each only as far as it takes:
//
//   - the columns in give wrap, one at a time in their order, each down to
//     its room; nothing of what they say is lost;
//   - the columns in drop are left out, in their order, the columns in give
//     first wrapping on together to the width one names;
//   - the columns in give wrap on together, down to wrapFloor;
//   - the columns in cut are cut to cutSoft;
//   - and then the columns in give, and last those in cut, go down to their
//     headers.
//
// A table with steps of its own is cut in those instead. Within a stage the
// widest column goes first, a tie going to the one named first. A cut line
// keeps its start, as many whole words of it as fit (see cutCell), and ends
// in the "…" that says more was there. A header is never cut, so a column is
// never narrower than its own; a table still too wide after all of it is
// drawn as narrow as it goes.
//
// left is the headers of the columns left out, in the order they were.
func (g *grid) fit() (header []string, rows [][]string, left []string) {
	w := widths(g.header, g.rows)
	// A table with no columns, a plain one with no rows, has nothing to cut.
	if g.width <= 0 || len(w) == 0 || g.frame(w) <= g.width {
		return g.header, g.rows, nil
	}
	natural := slices.Clone(w)
	floor := make([]int, len(w))
	shown := make([]bool, len(w))
	wraps := make([]bool, len(w))
	for i := range w {
		floor[i], shown[i] = screenWidth(g.head(i)), true
	}
	var give []int
	for _, x := range g.give {
		wraps[x.col] = true
		give = append(give, x.col)
	}
	// shape is a cell of column i as it is drawn n wide.
	shape := func(i int, c string, head bool, n int) string {
		switch {
		case n >= natural[i]:
			return c
		case wraps[i] && !head:
			return wrapCell(c, n)
		case g.cutBy[i] != nil:
			return g.cutBy[i](c, n)
		default:
			return cutCell(c, n)
		}
	}
	// drawn is how wide column i comes out when it is drawn n wide: its
	// widest line, header included, which is n or less — a wrapped column
	// breaks where its words end, and a cut one at the space before the
	// word it would split.
	drawn := func(i, n int) int {
		m := 0
		measure := func(c string, head bool) {
			for line := range strings.SplitSeq(shape(i, c, head, n), "\n") {
				m = max(m, screenWidth(line))
			}
		}
		measure(g.head(i), true)
		for _, r := range g.rows {
			if i < len(r) {
				measure(r[i], false)
			}
		}
		return m
	}
	over := func() int {
		var on []int
		for i, c := range w {
			if shown[i] {
				on = append(on, c)
			}
		}
		return g.frame(on) - g.width
	}
	// shrink narrows the widest of cols until the table fits or each is down
	// to least: to what it comes out at, so a short column is never left out
	// for room a wrap already freed, or, in a table with steps, a column at a
	// time whatever it comes out at, what that frees going back below.
	measured := g.steps == nil
	shrink := func(cols []int, least int) {
		for over() > 0 {
			at := -1
			for _, j := range cols {
				if shown[j] && w[j] > max(least, floor[j]) && (at < 0 || w[j] > w[at]) {
					at = j
				}
			}
			if at < 0 {
				return
			}
			if d := drawn(at, w[at]-1); measured && d < w[at] {
				w[at] = d
			} else {
				w[at]--
			}
		}
	}
	// cut is the columns cut, in the order what is left over goes back to.
	cut := g.cut
	if !measured {
		cut = nil
		for _, step := range g.steps {
			shrink(step.cols, step.floor)
			for _, i := range step.cols {
				if !slices.Contains(cut, i) {
					cut = append(cut, i)
				}
			}
		}
	} else {
		for _, x := range g.give {
			shrink([]int{x.col}, x.room)
		}
		for _, x := range g.drop {
			if x.after > 0 {
				shrink(give, x.after)
			}
			if over() <= 0 {
				break
			}
			shown[x.col] = false
			left = append(left, g.header[x.col])
		}
		shrink(give, wrapFloor)
		shrink(g.cut, cutSoft)
		shrink(give, 0)
		shrink(g.cut, 0)
	}
	// What is left over goes back to what was cut, and then to the free
	// text, which reads in fewer lines the wider it is: each column as wide
	// as the room left lets it come out.
	got, slack := make([]int, len(w)), -over()
	for i := range w {
		if got[i] = drawn(i, w[i]); shown[i] {
			slack += w[i] - got[i]
		}
	}
	for _, i := range append(slices.Clone(cut), give...) {
		for n := natural[i]; n > w[i] && slack > 0; n-- {
			if d := drawn(i, n); d-got[i] <= slack {
				slack -= d - got[i]
				w[i], got[i] = n, d
				break
			}
		}
	}
	keep := func(cells []string, head bool) (out []string) {
		for i, c := range cells {
			if shown[i] {
				out = append(out, shape(i, c, head, w[i]))
			}
		}
		return out
	}
	header = keep(g.header, true)
	rows = make([][]string, len(g.rows))
	for ri, r := range g.rows {
		rows[ri] = keep(r, false)
	}
	return header, rows, left
}

// wrapCell wraps each line of a cell that is longer than n at its spaces.
func wrapCell(c string, n int) string {
	var out []string
	for line := range strings.SplitSeq(c, "\n") {
		out = append(out, wrapLine(line, n)...)
	}
	return strings.Join(out, "\n")
}

// wrapLine breaks a line wider than n columns at its spaces into lines no
// wider than n. Something in backticks — `rota login` — is one word, so a
// command is never split across lines unless it is wider than n. A word
// wider than n on its own is broken where it reads best (see breakAt), and
// its start goes on the line before it when it breaks within the room that
// line has left, so a short word is not left on a line by itself. Where it
// must be broken where n falls, a wide character or an emoji n would fall in
// the middle of goes to the next line whole. A line that fits is returned as
// it is.
func wrapLine(s string, n int) []string {
	if n <= 0 || screenWidth(s) <= n {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, word := range words(s) {
		for screenWidth(word) > n {
			r := []rune(word)
			if cur != "" {
				if k := breakAt(r, n-screenWidth(cur)-1); k > 0 {
					out = append(out, cur+" "+strings.TrimRight(string(r[:k]), " "))
					cur, word = "", strings.TrimLeft(string(r[k:]), " ")
					continue
				}
				out = append(out, cur)
				cur = ""
			}
			k := breakAt(r, n)
			if k == 0 {
				// One character at the least, so a line too narrow for even
				// a wide one still moves on.
				if k = prefix(r, n); k == 0 {
					k, _ = char(r, 0)
				}
			}
			out = append(out, strings.TrimRight(string(r[:k]), " "))
			word = strings.TrimLeft(string(r[k:]), " ")
		}
		switch {
		case word == "":
		case cur == "":
			cur = word
		case screenWidth(cur)+1+screenWidth(word) <= n:
			cur += " " + word
		default:
			out = append(out, cur)
			cur = word
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// words splits a line at its spaces, except inside backticks: a command
// quoted in a note is one word however many spaces it has.
func words(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '`':
			quoted = !quoted
			b.WriteRune(r)
		case unicode.IsSpace(r) && !quoted:
			if b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
			}
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// breakAt is how many of a word's runes go on the first of the lines it is
// broken across, when it is wider than n columns: after the last space that
// fits, which only a quoted command has; before an "@", so an address breaks
// into its name and its domain; or after the last ".", "/" or "-" that fits.
// It is 0 when the word has none of those within n, and is broken where n
// falls only if it must be.
func breakAt(r []rune, n int) int {
	// fit is how many runes there is room for; from here on every limit is
	// counted in runes.
	fit := prefix(r, n)
	// last is the index of the last of marks at or before limit, and never
	// the first rune, which would leave a line with nothing on it.
	last := func(marks string, limit int) int {
		for k := min(limit, len(r)-1); k > 0; k-- {
			if strings.ContainsRune(marks, r[k]) {
				return k
			}
		}
		return 0
	}
	if k := last(" ", fit-1); k > 0 {
		return k + 1
	}
	if k := last("@", fit); k > 0 {
		return k
	}
	if k := last("./-", fit-1); k > 0 {
		return k + 1
	}
	return 0
}

// cut shortens s to n columns when it is wider, the last of them an "…"
// that says more was there; the start is always kept, and a wide character
// or an emoji the cut would halve goes with the rest.
func cut(s string, n int) string {
	if screenWidth(s) > n {
		r := []rune(s)
		return strings.TrimRight(string(r[:prefix(r, n-1)]), " ") + "…"
	}
	return s
}

// cutStart cuts each line of a cell to n columns at its start rather than
// its end: the "…" comes first, and the end is kept, for a cell whose end
// says the most. Like cutCell it does not begin in part of a word when there
// is a space to go forward to, so "1h ago, session af7fda4d" at 20 is
// "…session af7fda4d"; a path has none, and is cut where n falls, a wide
// character or an emoji the cut would halve going with the rest (see
// suffix).
func cutStart(c string, n int) string {
	lines := strings.Split(c, "\n")
	for i, line := range lines {
		if screenWidth(line) <= n {
			continue
		}
		r := []rune(line)
		k := suffix(r, n-1) // how many runes of the end are kept
		end := string(r[len(r)-k:])
		lines[i] = "…" + strings.TrimLeft(end, " ")
		if k < 1 || r[len(r)-k] == ' ' || r[len(r)-k-1] == ' ' {
			continue // nothing kept, or cut between two words
		}
		if at := strings.Index(end, " "); at >= 0 {
			if kept := strings.TrimLeft(end[at:], " "); chars(kept) >= 2 {
				lines[i] = "…" + kept
			}
		}
	}
	return strings.Join(lines, "\n")
}

// cutCell cuts each line of a cell to n, as cut does, except where that
// would end the line in part of a word: then it is cut at the space before
// the word, so "5h 100% (4h 12m)" at 6 is "5h…" and never "5h 10…", which
// reads as a different number. A line with no space to go back to, or only
// one that leaves fewer than two letters, is cut where n falls.
func cutCell(c string, n int) string {
	lines := strings.Split(c, "\n")
	for i, line := range lines {
		lines[i] = cut(line, n)
		r := []rune(line)
		k := prefix(r, n-1) // how many runes cut keeps
		if screenWidth(line) <= n || k < 1 || r[k-1] == ' ' || r[k] == ' ' {
			continue // nothing cut, or cut between two words
		}
		head, kept := string(r[:k]), ""
		if at := strings.LastIndex(head, " "); at > 0 {
			kept = strings.TrimRight(head[:at], " ")
		}
		if chars(kept) >= 2 {
			lines[i] = kept + "…"
		}
	}
	return strings.Join(lines, "\n")
}

// render draws the table. Every line is exactly as wide as the frame, and
// none ends in padding: the border is the last thing on it. It returns the
// headers of the columns left out to fit the terminal, for a note to name
// (see fit).
func (g *grid) render(out io.Writer) []string {
	if g.plain {
		g.renderPlain(out)
		return nil
	}
	header, rows, gone := g.fit()
	w := widths(header, rows)
	rule := func(left, mid, right string) string {
		parts := make([]string, len(w))
		for i, n := range w {
			parts[i] = strings.Repeat("─", n+2)
		}
		return left + strings.Join(parts, mid) + right
	}
	line := func(cells []string) string {
		var b strings.Builder
		for i, n := range w {
			c := ""
			if i < len(cells) {
				c = cells[i]
			}
			b.WriteString("│ ")
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", n-screenWidth(c)+1))
		}
		b.WriteString("│")
		return b.String()
	}
	ruled := false
	for _, r := range rows {
		for _, c := range r {
			ruled = ruled || strings.Contains(c, "\n")
		}
	}

	fmt.Fprintln(out, rule("┌", "┬", "┐"))
	fmt.Fprintln(out, line(header))
	fmt.Fprintln(out, rule("├", "┼", "┤"))
	for ri, r := range rows {
		if ri > 0 && ruled {
			fmt.Fprintln(out, rule("├", "┼", "┤"))
		}
		for _, cells := range rowLines(r) {
			fmt.Fprintln(out, line(cells))
		}
	}
	fmt.Fprintln(out, rule("└", "┴", "┘"))
	return gone
}

// rowLines is a row as it is drawn, line by line: each cell split at its
// newlines, and a cell with fewer lines than the row's tallest empty on the
// lines after its last.
func rowLines(r []string) [][]string {
	split := make([][]string, len(r))
	height := 1
	for i, c := range r {
		split[i] = strings.Split(c, "\n")
		height = max(height, len(split[i]))
	}
	out := make([][]string, height)
	for k := range out {
		out[k] = make([]string, len(r))
		for i := range r {
			if k < len(split[i]) {
				out[k][i] = split[i][k]
			}
		}
	}
	return out
}
