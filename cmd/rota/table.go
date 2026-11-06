package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
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
	// — a place, an id, a percentage — which are cut only once every other
	// column is down to its "…".
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
// width is a terminal's. Without one, it is printed exactly as it is.
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

// minWrap is how narrow a column that wraps gets before the next one gives
// way, and wrapFloor how narrow any of them gets at all: below it a sentence
// turns into a column of single words, and cutting reads better.
const (
	minWrap   = 16
	wrapFloor = 10
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
// then together down to wrapFloor; nothing of what they say is lost. Only a
// table that still cannot fit then has its widest columns cut, a column at a
// time, each cut line ending in "…" — so a cell always keeps its start.
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
	// widest is the widest of these columns that is wider than floor, or -1.
	// A tie goes to the later column, which is the one with less to say.
	widest := func(cols []int, floor func(int) int) int {
		at := -1
		for _, j := range cols {
			if w[j] > floor(j) && (at < 0 || w[j] >= w[at]) {
				at = j
			}
		}
		return at
	}
	// Every column that wraps has given way to its minimum and the table
	// still does not fit: they go on wrapping, the widest first, down to
	// wrapFloor, which costs more lines and loses nothing.
	for ; over > 0; over-- {
		i := widest(g.give, func(j int) int { return min(natural[j], wrapFloor) })
		if i < 0 {
			break
		}
		w[i]--
	}
	// Only now is anything cut, the widest column first — the columns in
	// keep last of all — down to a first character and the "…" that says
	// more was there.
	var rest, all []int
	for j := range w {
		all = append(all, j)
		if !slices.Contains(g.keep, j) {
			rest = append(rest, j)
		}
	}
	cutFloor := func(int) int { return 2 }
	for ; over > 0; over-- {
		i := widest(rest, cutFloor)
		if i < 0 {
			i = widest(all, cutFloor)
		}
		if i < 0 {
			break // nothing left to take: the table is drawn as narrow as it goes
		}
		w[i]--
	}
	wraps := make([]bool, len(w))
	for _, i := range g.give {
		wraps[i] = w[i] >= min(natural[i], wrapFloor)
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
	header = shape(g.header, true)
	rows = make([][]string, len(g.rows))
	for ri, r := range g.rows {
		rows[ri] = shape(r, false)
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

// wrapLine breaks a line longer than n at its spaces into lines no longer
// than n. A word longer than n on its own — an address, a path — is broken
// where n falls, since there is nowhere better to break it. A line that fits
// is returned as it is.
func wrapLine(s string, n int) []string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return []string{s}
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(s) {
		for utf8.RuneCountInString(word) > n {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			r := []rune(word)
			out = append(out, string(r[:n]))
			word = string(r[n:])
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

// cutCell shortens each line of a cell that is longer than n to n, the last
// of them an ellipsis.
func cutCell(c string, n int) string {
	lines := strings.Split(c, "\n")
	for i, line := range lines {
		r := []rune(line)
		if len(r) > n {
			lines[i] = strings.TrimRight(string(r[:max(n-1, 0)]), " ") + "…"
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
