package main

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
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
}

func (g *grid) add(cells ...string) { g.rows = append(g.rows, cells) }

// widths is the widest line in each column, header included.
func (g *grid) widths() []int {
	w := make([]int, len(g.header))
	measure := func(cells []string) {
		for i, c := range cells {
			for line := range strings.SplitSeq(c, "\n") {
				w[i] = max(w[i], utf8.RuneCountInString(line))
			}
		}
	}
	measure(g.header)
	for _, r := range g.rows {
		measure(r)
	}
	return w
}

// render draws the table. Every line is exactly as wide as the frame, and
// none ends in padding: the border is the last thing on it.
func (g *grid) render(out io.Writer) {
	w := g.widths()
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
	for _, r := range g.rows {
		for _, c := range r {
			ruled = ruled || strings.Contains(c, "\n")
		}
	}

	fmt.Fprintln(out, rule("┌", "┬", "┐"))
	fmt.Fprintln(out, line(g.header))
	fmt.Fprintln(out, rule("├", "┼", "┤"))
	for ri, r := range g.rows {
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
