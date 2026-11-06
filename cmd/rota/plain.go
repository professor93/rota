package main

import (
	"fmt"
	"io"
	"strings"
)

// stage is a step in fitting a table to its terminal: its columns are cut,
// the widest of them first, until each is down to floor.
type stage struct {
	cols  []int
	floor int
}

// renderPlain draws a plain table (see grid.plain) one line per row, lined
// up the way text/tabwriter lines them up: a cell is padded by the columns
// it takes on screen (see screenWidth) where tabwriter counts its runes, so
// a name in Chinese lines up too, and a control character in a cell, which
// would move the cursor where no count can follow, is a space (see
// printable). A table of one-column characters with no control character in
// it that needs no fitting comes out byte for byte as tabwriter drew it. A
// cell that spans lines leaves the other cells of its later lines empty.
func (g *grid) renderPlain(out io.Writer) {
	clean := *g
	clean.rows = make([][]string, len(g.rows))
	for i, r := range g.rows {
		clean.rows[i] = make([]string, len(r))
		for j, c := range r {
			clean.rows[i][j] = printable(c)
		}
	}
	_, rows := clean.fit()
	var lines [][]string
	for _, r := range rows {
		lines = append(lines, rowLines(r)...)
	}
	w := widths(nil, lines)
	for _, cells := range lines {
		var b strings.Builder
		b.WriteString("  ")
		for i, c := range cells {
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", w[i]-screenWidth(c)+2))
			}
		}
		fmt.Fprintln(out, b.String())
	}
}
