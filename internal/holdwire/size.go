package holdwire

// The largest terminal rota makes, either way.
const (
	MaxCols = 1000
	MaxRows = 1000
)

// BoundSize keeps a terminal to a size a terminal can be. Zero is the
// default rather than an error — a client that says nothing means the
// ordinary eighty by twenty-four — and the ceiling is there because the
// numbers arrive from whoever is connected. The server's terminals, a
// holder's pty and its screen are all held to it, so it is said once.
func BoundSize(cols, rows uint16) (uint16, uint16) {
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	return min(cols, MaxCols), min(rows, MaxRows)
}
