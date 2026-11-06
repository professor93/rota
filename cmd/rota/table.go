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
	// the order they give way. The rest keep their width until nothing else
	// is left to give.
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
// goes, enough for "5h 3%…", and cutHard a first character and the "…".
const (
	minWrap   = 16
	wrapFloor = 10
	hardWrap  = 4
	cutSoft   = 6
	cutHard   = 2
)

func (g *grid) add(cells ...string) { g.rows = append(g.rows, cells) }

// widths is the widest line in each column, header included.
func widths(header []string, rows [][]string) []int {
	w := make([]int, len(header))
	measure := func(cells []string) {
		for i, c := range cells {
			for line := range strings.SplitSeq(c, "\n") {
				w[i] = max(w[i], utf8.RuneCountInString(line))
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
	//   - the others cut to cutSoft, which keeps a reading's percentage;
	//   - the wrapping columns again, down to hardWrap; still nothing lost;
	//   - the others cut to cutHard, keep last of all;
	//   - and only then the wrapping columns cut, to cutHard as well.
	//
	// A cut line keeps its start and ends in the "…" that says more was
	// there. What is still too wide after all of it is drawn as narrow as
	// it goes.
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
	shape := func(cells []string, head bool) []string {
		out := make([]string, len(cells))
		for i, c := range cells {
			switch {
			case i >= len(w) || w[i] >= natural[i]:
				out[i] = c
			case wraps[i] && !head:
				out[i] = wrapCell(c, w[i])
			default:
				out[i] = cutCell(c, w[i])
			}
		}
		return out
	}
	draw := func() {
		header = shape(g.header, true)
		rows = make([][]string, len(g.rows))
		for ri, r := range g.rows {
			rows[ri] = shape(r, false)
		}
	}
	draw()
	// A wrapped column is often narrower than the width it was given — its
	// lines break where its words end — and what it leaves over goes back
	// to the cut columns, in the order they were cut, so nothing is cut that
	// the terminal had room for.
	got := widths(header, rows)
	slack := g.width - frame(got)
	if slack <= 0 {
		return header, rows
	}
	copy(w, got)
	for _, cols := range [][]int{rest, g.keep, g.give} {
		for _, i := range cols {
			if !wraps[i] && w[i] < natural[i] {
				add := min(slack, natural[i]-w[i])
				w[i] += add
				slack -= add
			}
		}
	}
	draw()
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

// wrapLine breaks a line longer than n at its spaces into lines no longer
// than n. Something in backticks — `rota login` — is one word, so a command
// is never split across lines. A word longer than n on its own is broken
// where it reads best (see breakAt). A line that fits is returned as it is.
func wrapLine(s string, n int) []string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, word := range words(s) {
		for utf8.RuneCountInString(word) > n {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			r := []rune(word)
			k := breakAt(r, n)
			out = append(out, strings.TrimRight(string(r[:k]), " "))
			word = strings.TrimLeft(string(r[k:]), " ")
		}
		switch {
		case word == "":
		case cur == "":
			cur = word
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= n:
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
// broken across, when it is longer than n: after the last space that fits,
// which only a quoted command has; before an "@", so an address breaks into
// its name and its domain; after the last ".", "/" or "-" that fits; and
// only failing all of those, where n falls.
func breakAt(r []rune, n int) int {
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
	if k := last(" ", n-1); k > 0 {
		return k + 1
	}
	if k := last("@", n); k > 0 {
		return k
	}
	if k := last("./-", n-1); k > 0 {
		return k + 1
	}
	return n
}

// cut shortens s to n runes when it is longer, the last of them an "…" that
// says more was there; the start is always kept.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return strings.TrimRight(string(r[:max(n-1, 0)]), " ") + "…"
	}
	return s
}

// cutCell cuts each line of a cell to n.
func cutCell(c string, n int) string {
	lines := strings.Split(c, "\n")
	for i, line := range lines {
		lines[i] = cut(line, n)
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
			b.WriteString(strings.Repeat(" ", n-utf8.RuneCountInString(c)+1))
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
