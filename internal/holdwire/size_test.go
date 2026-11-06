package holdwire

import "testing"

// A size from whoever is connected is held to one a terminal can be: nothing
// said is the ordinary 80×24, and nothing is bigger than 1000 either way.
func TestBoundSize(t *testing.T) {
	for _, c := range []struct{ cols, rows, wantCols, wantRows uint16 }{
		{0, 0, 80, 24},
		{0, 50, 80, 50},
		{120, 0, 120, 24},
		{1, 1, 1, 1},
		{1000, 1000, 1000, 1000},
		{1001, 5000, 1000, 1000},
		{65535, 40, 1000, 40},
	} {
		if cols, rows := BoundSize(c.cols, c.rows); cols != c.wantCols || rows != c.wantRows {
			t.Errorf("BoundSize(%d, %d) = %d×%d, want %d×%d", c.cols, c.rows, cols, rows, c.wantCols, c.wantRows)
		}
	}
}
