package holdwire

// The largest terminal rota makes, either way.
const (
	MaxCols = 1000
	MaxRows = 1000
)

// MaxScreenCells is the largest terminal, in cells, a holder keeps a screen
// for. A pty may be as large as MaxCols by MaxRows, but what the emulator
// holds and what each line of output costs it grow with its area: at
// 1000×1000 one screen held 235 MB and took four seconds over 32 KiB of short
// lines, against 52 MB and two seconds at this size. A larger session is
// replayed from its ring rather than repainted.
const MaxScreenCells = 200_000

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

// ScreenFits says whether a holder keeps a screen for a terminal of this
// size: whether it is at most MaxScreenCells.
func ScreenFits(cols, rows uint16) bool { return int(cols)*int(rows) <= MaxScreenCells }
