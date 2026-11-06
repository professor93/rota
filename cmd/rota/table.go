package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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
	// give is the columns that wrap their cells to fit a narrow terminal, in
	// the order they give way. They wrap down to wrapFloor before anything is
	// cut, further once the columns in neither list are cut to cutSoft, and
	// are cut themselves last of all, once every other column is down to its
	// "…" (see fit).
	give []int
	// keep is the columns whose content is short and always the same shape
	// — a place, an id, a percentage — which are cut only once every column
	// but give is down to its "…", in the order they give way.
	keep []int
}

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
// Without one, it is printed exactly as it is.
func say(out io.Writer, width int, text string) {
	if width > 0 {
		var lines []string
		for line := range strings.SplitSeq(text, "\n") {
			lines = append(lines, wrapLine(line, width)...)
		}
		text = strings.Join(lines, "\n")
	}
	io.WriteString(out, text)
}

// How narrow columns get, and in which order (see fit). A column that wraps
// goes down to minWrap before the next one gives way, and to wrapFloor
// before anything else is cut; below hardWrap it is cut too, since three
// letters a line is no longer reading. cutSoft is as far as the first cut
// goes: "5h 100%…", so a window with a two-letter name keeps its percentage
// however spent it is, while a longer one, "Fable 48%", is cut to its name.
// cutHard is a first character and the "…".
const (
	minWrap   = 16
	wrapFloor = 10
	hardWrap  = 4
	cutSoft   = 8
	cutHard   = 2
)

func (g *grid) add(cells ...string) { g.rows = append(g.rows, cells) }

// widths is the widest line in each column, header included, in the
// columns it takes on screen (see screenWidth).
func widths(header []string, rows [][]string) []int {
	w := make([]int, len(header))
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

// frame is how wide a table with these column widths is drawn: each column
// is a border, a space, its content and a space, and one border closes it.
func frame(w []int) int {
	n := 1
	for _, c := range w {
		n += c + 3
	}
	return n
}

// fit is the table's cells as drawn: as they are when the table fits, or
// when no width was asked for, and otherwise narrowed until it does.
//
// The columns in give wrap first, in their order, each down to minWrap, and
// nothing of what they say is lost. The rest of the stages are below.
func (g *grid) fit() (header []string, rows [][]string) {
	w := widths(g.header, g.rows)
	if g.width <= 0 || frame(w) <= g.width {
		return g.header, g.rows
	}
	natural := slices.Clone(w)
	over := frame(w) - g.width
	for _, i := range g.give {
		take := min(over, max(w[i]-min(natural[i], minWrap), 0))
		w[i] -= take
		over -= take
	}
	// The other columns: the ones nobody named, which say the most after
	// give (USAGE), and then keep.
	var rest []int
	for j := range w {
		if !slices.Contains(g.give, j) && !slices.Contains(g.keep, j) {
			rest = append(rest, j)
		}
	}
	// Then one column at a time, the widest of each stage's columns first —
	// a tie going to the one named first, which is the one meant to give
	// way first — until each is down to the stage's floor:
	//
	//   - the wrapping columns, together, down to wrapFloor; nothing lost;
	//   - the columns in neither list (USAGE) cut to cutSoft;
	//   - the wrapping columns again, down to hardWrap; still nothing lost;
	//   - the columns in neither list cut to cutHard, then keep;
	//   - and only then the wrapping columns cut, to cutHard as well.
	//
	// A cut line keeps its start, as many whole words of it as fit (see
	// cutCell), and ends in the "…" that says more was there. What is still
	// too wide after all of it is drawn as narrow as it goes.
	for _, stage := range []struct {
		cols  []int
		floor int
	}{
		{g.give, wrapFloor},
		{rest, cutSoft},
		{g.give, hardWrap},
		{rest, cutHard},
		{g.keep, cutHard},
		{g.give, cutHard},
	} {
		for ; over > 0; over-- {
			at := -1
			for _, j := range stage.cols {
				if w[j] > stage.floor && (at < 0 || w[j] > w[at]) {
					at = j
				}
			}
			if at < 0 {
				break
			}
			w[at]--
		}
	}
	wraps := make([]bool, len(w))
	for _, i := range g.give {
		wraps[i] = w[i] >= hardWrap
	}
	// shape is a cell of column i as it is drawn n wide. A wrapping column's
	// header is cut, not wrapped: it is one word.
	shape := func(i int, c string, head bool, n int) string {
		switch {
		case n >= natural[i]:
			return c
		case wraps[i] && !head:
			return wrapCell(c, n)
		default:
			return cutCell(c, n)
		}
	}
	// drawn is how wide column i comes out when it is drawn n wide: its
	// widest line, header included, which is n or less.
	drawn := func(i, n int) int {
		m := 0
		measure := func(c string, head bool) {
			for line := range strings.SplitSeq(shape(i, c, head, n), "\n") {
				m = max(m, screenWidth(line))
			}
		}
		measure(g.header[i], true)
		for _, r := range g.rows {
			if i < len(r) {
				measure(r[i], false)
			}
		}
		return m
	}
	// A column often comes out narrower than the width it was given: a
	// wrapped one breaks where its words end, and a cut one at the space
	// before the word it would split. When anything is cut, what that leaves
	// over goes back to the cut columns, in the order they were cut, each
	// made as wide as the room left lets it come out, and then to the
	// wrapping columns the same way. A table with a cut in it is then as
	// wide as the terminal, unless what is left is less than any column
	// needs to show more.
	got := make([]int, len(w))
	cuts := false
	for i := range w {
		got[i] = drawn(i, w[i])
		cuts = cuts || w[i] < natural[i] && (!wraps[i] || screenWidth(g.header[i]) > w[i])
	}
	if slack := g.width - frame(got); cuts && slack > 0 {
		var order []int
		for _, cols := range [][]int{rest, g.keep, g.give} {
			for _, i := range cols {
				if !wraps[i] {
					order = append(order, i)
				}
			}
		}
		for _, i := range g.give {
			if wraps[i] {
				order = append(order, i)
			}
		}
		for _, i := range order {
			for n := natural[i]; n > w[i] && slack > 0; n-- {
				if d := drawn(i, n); d-got[i] <= slack {
					slack -= d - got[i]
					w[i], got[i] = n, d
					break
				}
			}
		}
	}
	header = make([]string, len(g.header))
	for i, c := range g.header {
		header[i] = shape(i, c, true, w[i])
	}
	rows = make([][]string, len(g.rows))
	for ri, r := range g.rows {
		rows[ri] = make([]string, len(r))
		for i, c := range r {
			rows[ri][i] = shape(i, c, false, w[i])
		}
	}
	return header, rows
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
		if utf8.RuneCountInString(kept) >= 2 {
			lines[i] = kept + "…"
		}
	}
	return strings.Join(lines, "\n")
}

// render draws the table. Every line is exactly as wide as the frame, and
// none ends in padding: the border is the last thing on it.
func (g *grid) render(out io.Writer) {
	header, rows := g.fit()
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
		split := make([][]string, len(r))
		height := 1
		for i, c := range r {
			split[i] = strings.Split(c, "\n")
			height = max(height, len(split[i]))
		}
		for k := range height {
			cells := make([]string, len(r))
			for i := range r {
				if k < len(split[i]) {
					cells[i] = split[i][k]
				}
			}
			fmt.Fprintln(out, line(cells))
		}
	}
	fmt.Fprintln(out, rule("└", "┴", "┘"))
}
