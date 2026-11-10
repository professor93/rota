package attach

// viewport chooses the part of a session of sessCols by sessRows that a
// window of winCols by winRows shows while it is cropped: the column x and
// row y of the session, from 0, that the window's top left corner shows.
//
// The window's last row is the note, so it shows winRows-1 rows of the
// session. The crop is anchored at the bottom, where Claude Code draws its
// prompt, and moves up only as far as keeps the cursor's row, curRow, in
// view. Sideways it starts where it was, at prevX, moves only as far as
// keeps the cursor's column, curCol, in view, and never shows past either
// edge of the session.
func viewport(sessCols, sessRows, winCols, winRows, curRow, curCol, prevX int) (x, y int) {
	shown := max(winRows-1, 0)
	y = max(0, sessRows-shown)
	if shown > 0 && curRow >= 0 && curRow < y {
		y = curRow
	}
	x = prevX
	if winCols > 0 {
		if curCol < x {
			x = curCol
		}
		if curCol >= x+winCols {
			x = curCol - winCols + 1
		}
	}
	x = min(max(x, 0), max(0, sessCols-winCols))
	return x, y
}
