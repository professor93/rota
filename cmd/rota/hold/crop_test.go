package hold

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// cropScreen is a 20x5 screen with text on the second row, a red word and two
// wide characters on the third and fourth, and the cursor at row 2, column 3,
// counted from 0.
func cropScreen(t *testing.T) *Screen {
	t.Helper()
	s := NewScreen(20, 5)
	t.Cleanup(s.Close)
	s.Write([]byte("\x1b[2;1H0123456789abcdefghij"))
	// "red" from column 5, the crop's first, and 世 at columns 14 and 15,
	// which the crop's right edge cuts.
	s.Write([]byte("\x1b[3;6H\x1b[31mred\x1b[0m\x1b[3;15H世"))
	// 世 at columns 4 and 5, which the crop's left edge cuts, then "ok".
	s.Write([]byte("\x1b[4;5H世ok"))
	s.Write([]byte("\x1b[3;4H"))
	return s
}

// cropCheck says what is wrong with one row of a window, or "".
func cropCheck(row []byte, cols int) string {
	switch {
	case !bytes.HasPrefix(row, []byte("\x1b[0m")):
		return "does not start from ESC[0m"
	case !bytes.HasSuffix(row, []byte("\x1b[0m")):
		return "does not end with ESC[0m"
	case bytes.ContainsAny(row, "\r\n\b"):
		return "moves the cursor"
	}
	for _, f := range []string{"H", "A", "B", "C", "D", "G", "d", "K", "J"} {
		for i := 0; i < len(row); i++ {
			if row[i] != 0x1b {
				continue
			}
			j := i + 2
			for j < len(row) && (row[j] >= '0' && row[j] <= '9' || row[j] == ';') {
				j++
			}
			if j < len(row) && string(row[j]) == f {
				return "moves the cursor or erases: " + string(row[i:j+1])
			}
		}
	}
	text := ansi.Strip(string(row))
	if strings.TrimRight(text, " ") != text {
		return "ends in blanks"
	}
	if w := ansi.StringWidth(text); w > cols {
		return "is wider than the window"
	}
	return ""
}

func TestCropShowsAWindowOntoTheScreen(t *testing.T) {
	s := cropScreen(t)
	w, ok := s.Crop(5, 1, 10, 3)
	if !ok {
		t.Fatal("Crop said there is no screen")
	}
	if len(w.Rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(w.Rows))
	}
	for i, r := range w.Rows {
		if why := cropCheck(r, 10); why != "" {
			t.Errorf("row %d %q %s", i, r, why)
		}
	}
	if got := ansi.Strip(string(w.Rows[0])); got != "56789abcde" {
		t.Errorf("row 0 reads %q, want %q", got, "56789abcde")
	}
	if !bytes.HasPrefix(w.Rows[1], []byte("\x1b[0m\x1b[31mred")) {
		t.Errorf("row 1 %q does not draw red from column 0 in red", w.Rows[1])
	}
	// "red", six blank cells, and the 世 the edge cuts as a space, which
	// is a blank and so is left off.
	if got := ansi.Strip(string(w.Rows[1])); got != "red" {
		t.Errorf("row 1 reads %q, want %q", got, "red")
	}
	if bytes.Contains(w.Rows[1], []byte("世")) {
		t.Errorf("row 1 %q draws a wide character the edge cuts", w.Rows[1])
	}
	if got := ansi.Strip(string(w.Rows[2])); got != " ok" {
		t.Errorf("row 2 reads %q, want %q: the cut 世 a space", got, " ok")
	}
	if w.Row != 1 || w.Col != -2 || w.CursorIn {
		t.Errorf("cursor Row %d Col %d CursorIn %v, want 1 -2 false", w.Row, w.Col, w.CursorIn)
	}

	// The wide character whole, and the cursor inside.
	s.Write([]byte("\x1b[3;8H"))
	w, _ = s.Crop(5, 1, 11, 3)
	if got := ansi.Strip(string(w.Rows[1])); got != "red      世" {
		t.Errorf("row 1 at 11 wide reads %q, want the whole 世", got)
	}
	if w.Row != 1 || w.Col != 2 || !w.CursorIn {
		t.Errorf("cursor Row %d Col %d CursorIn %v, want 1 2 true", w.Row, w.Col, w.CursorIn)
	}
	if c, r := s.Size(); c != 20 || r != 5 {
		t.Errorf("Size is %dx%d, want 20x5", c, r)
	}
}

func TestCropPastTheScreenIsBlank(t *testing.T) {
	s := cropScreen(t)
	s.Write([]byte("\x1b[5;16Hend"))
	w, ok := s.Crop(15, 3, 10, 4)
	if !ok {
		t.Fatal("Crop said there is no screen")
	}
	if len(w.Rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(w.Rows))
	}
	for i, r := range w.Rows {
		if why := cropCheck(r, 10); why != "" {
			t.Errorf("row %d %q %s", i, r, why)
		}
	}
	if got := ansi.Strip(string(w.Rows[1])); got != "end" {
		t.Errorf("row 1 reads %q, want %q", got, "end")
	}
	for _, i := range []int{2, 3} {
		if got := ansi.Strip(string(w.Rows[i])); got != "" {
			t.Errorf("row %d, past the screen, reads %q", i, got)
		}
	}
	// Columns past the screen too.
	w, _ = s.Crop(18, 0, 10, 1)
	if got := ansi.Strip(string(w.Rows[0])); got != "" {
		t.Errorf("a row past the right edge reads %q", got)
	}
}

func TestCropWithoutAnEmulatorIsNothing(t *testing.T) {
	s := NewScreen(20, 5)
	s.Close()
	if w, ok := s.Crop(0, 0, 10, 3); ok || w.Rows != nil || w.CursorIn {
		t.Errorf("a closed screen cropped to %+v, %v", w, ok)
	}
	if c, r := s.Size(); c != 0 || r != 0 {
		t.Errorf("a closed screen's size is %dx%d", c, r)
	}
	if w, ok := noScreen().Crop(0, 0, 10, 3); ok || w.Rows != nil {
		t.Errorf("no screen cropped to %+v, %v", w, ok)
	}
}
