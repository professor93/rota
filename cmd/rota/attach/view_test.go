package attach

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/professor93/rota/internal/vtscan"
)

// viewNote is a note that says all four sizes and is 60 characters long.
func viewNote(sc, sr, wc, wr int) string {
	s := fmt.Sprintf("%d×%d session in a %d×%d window ", sc, sr, wc, wr)
	return s + strings.Repeat("-", 60-utf8.RuneCountInString(s))
}

func testView(t *testing.T, sc, sr, wc, wr int) (*view, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	v := newView(&out, sc, sr, wc, wr, viewNote)
	t.Cleanup(v.close)
	return v, &out
}

// xLines fills a session of cols by rows with x, one line at a time.
func xLines(v *view, cols, rows int) {
	for r := range rows {
		v.output([]byte(fmt.Sprintf("\x1b[%d;1H%s", r+1, strings.Repeat("x", cols))))
	}
}

// rowAt is where the view starts drawing a row.
var rowAt = regexp.MustCompile(`\x1b\[(\d+);1H`)

// drawnRows splits what the view wrote at each row it drew, by row number.
func drawnRows(out []byte) map[string][]string {
	rows := map[string][]string{}
	idx := rowAt.FindAllSubmatchIndex(out, -1)
	for i, m := range idx {
		end := len(out)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		r := string(out[m[2]:m[3]])
		rows[r] = append(rows[r], string(out[m[1]:end]))
	}
	return rows
}

func TestViewportFollowsTheCursor(t *testing.T) {
	for _, c := range []struct {
		name                   string
		sc, sr, wc, wr         int
		row, col, prev, wx, wy int
	}{
		{"bottom-anchored", 100, 30, 40, 12, 29, 5, 0, 0, 19},
		{"right to the cursor", 100, 30, 40, 12, 29, 70, 0, 31, 19},
		{"up to the cursor", 100, 30, 40, 12, 10, 0, 0, 0, 10},
		{"as wide as the session", 100, 30, 100, 12, 29, 70, 0, 0, 19},
		{"stays where the cursor is in view", 100, 30, 40, 12, 29, 50, 31, 31, 19},
		{"left to the cursor", 100, 30, 40, 12, 29, 20, 31, 20, 19},
		{"never past the right edge", 100, 30, 40, 12, 29, 99, 90, 60, 19},
		{"a window taller than the session", 100, 30, 40, 50, 29, 5, 0, 0, 0},
	} {
		x, y := viewport(c.sc, c.sr, c.wc, c.wr, c.row, c.col, c.prev)
		if x != c.wx || y != c.wy {
			t.Errorf("%s: viewport gave (%d,%d), want (%d,%d)", c.name, x, y, c.wx, c.wy)
		}
	}
}

func TestPassThroughWritesTheBytesAsTheyCame(t *testing.T) {
	v, out := testView(t, 80, 24, 80, 24)
	if out.Len() != 0 || v.cropped() {
		t.Fatalf("a view that fits wrote %q, cropped %v", out, v.cropped())
	}
	v.output([]byte("hi\x1b[1m"))
	if got := out.String(); got != "hi\x1b[1m" {
		t.Fatalf("output wrote %q, want the bytes as they came", got)
	}
	if v.flush() {
		t.Error("flush drew in pass-through")
	}
	out.Reset()
	v.redraw()
	if !bytes.HasPrefix(out.Bytes(), vtscan.ResetModes()) || !bytes.Contains(out.Bytes(), []byte("hi")) {
		t.Errorf("redraw wrote %q, want a repaint with hi", out)
	}
	if !v.screenOK() {
		t.Error("an 80×24 session has no screen")
	}
}

func TestACropNeverWritesPastTheWindow(t *testing.T) {
	v, out := testView(t, 100, 30, 40, 12)
	if !v.cropped() {
		t.Fatal("a 100×30 session in a 40×12 window is not cropped")
	}
	xLines(v, 100, 30)
	if !v.flush() {
		t.Fatal("flush drew nothing")
	}
	all := out.Bytes()
	if bytes.Contains(all, []byte(strings.Repeat("x", 41))) || bytes.ContainsAny(all, "\r\n") {
		t.Fatal("the session's output reached the window raw")
	}
	rows := drawnRows(all)
	for r, segs := range rows {
		for _, s := range segs {
			if w := ansi.StringWidth(s); w > 40 {
				t.Errorf("row %s holds %d cells: %q", r, w, s)
			}
		}
	}
	for r := 1; r <= 11; r++ {
		segs := rows[fmt.Sprint(r)]
		if len(segs) == 0 || ansi.Strip(segs[len(segs)-1]) != strings.Repeat("x", 40) {
			t.Errorf("row %d drew %q, want forty x", r, segs)
		}
	}
	notes := rows["12"]
	if len(notes) == 0 {
		t.Fatalf("no note on the last row in %q", all)
	}
	note := notes[len(notes)-1]
	want := ansi.Truncate(viewNote(100, 30, 40, 12), 40, "")
	if !strings.HasPrefix(note, "\x1b[7m") || ansi.Strip(note) != want || !strings.HasPrefix(want, "100×30") {
		t.Errorf("the note row is %q, want %q in reverse", note, want)
	}
}

func TestACropDrawsOnlyWhatChanged(t *testing.T) {
	v, out := testView(t, 100, 30, 40, 12)
	xLines(v, 100, 30)
	v.output([]byte("\x1b[30;1H"))
	v.flush()
	out.Reset()
	if v.flush() || out.Len() != 0 {
		t.Fatalf("a flush with nothing new wrote %q", out)
	}
	v.output([]byte("\x1b[30;1Hz"))
	if out.Len() != 0 {
		t.Fatalf("output wrote %q while cropped", out)
	}
	if !v.flush() {
		t.Fatal("flush drew nothing")
	}
	rows := drawnRows(out.Bytes())
	if len(rows) != 2 || len(rows["11"]) != 1 || len(rows["12"]) != 1 {
		t.Fatalf("flush drew rows %q, want row 11 and the note", rows)
	}
	if got := ansi.Strip(rows["11"][0]); got != "z"+strings.Repeat("x", 39) {
		t.Errorf("row 11 reads %q", got)
	}
	if !strings.HasSuffix(out.String(), "\x1b[11;2H\x1b[?25h") {
		t.Errorf("the cursor is not after the z: %q", out)
	}
	// A redraw draws it all again on a cleared window.
	out.Reset()
	v.redraw()
	if rows := drawnRows(out.Bytes()); len(rows) != 12 || !strings.Contains(out.String(), "\x1b[2J") {
		t.Errorf("redraw drew %d rows: %q", len(rows), out)
	}
	// The cursor hidden, or out of the crop, is hidden.
	out.Reset()
	v.output([]byte("\x1b[?25l"))
	v.flush()
	if !strings.HasSuffix(out.String(), "\x1b[?25l") {
		t.Errorf("a hidden cursor is shown: %q", out)
	}
}

func TestACropTurnsTheMouseOffAndKeepsTheKeys(t *testing.T) {
	v, out := testView(t, 100, 30, 40, 12)
	v.output([]byte("\x1b[?1002h\x1b[?1006h\x1b[?2004h\x1b[?1h"))
	v.flush()
	got := out.String()
	for _, want := range []string{"\x1b[?2004h", "\x1b[?1h"} {
		if !strings.Contains(got, want) {
			t.Errorf("the crop never wrote %q: %q", want, got)
		}
	}
	for _, not := range []string{"\x1b[?1002h", "\x1b[?1006h"} {
		if strings.Contains(got, not) {
			t.Errorf("the crop wrote %q: %q", not, got)
		}
	}
	// Leaving the crop gives the mouse back.
	out.Reset()
	v.window(100, 30)
	if !strings.Contains(out.String(), "\x1b[?1002h") {
		t.Errorf("leaving the crop wrote %q, with no mouse", out)
	}
}

func TestTheWindowGrowingLeavesTheCrop(t *testing.T) {
	v, out := testView(t, 100, 30, 40, 12)
	if !strings.HasPrefix(out.String(), "\x1b[?1049h") {
		t.Errorf("entering the crop wrote %q, not the alternate screen first", out)
	}
	v.output([]byte("hello"))
	v.flush()
	out.Reset()
	v.window(100, 30)
	if v.cropped() {
		t.Fatal("a window as large as the session is still cropped")
	}
	rest, ok := bytes.CutPrefix(out.Bytes(), []byte("\x1b[?1049l"))
	if !ok || !bytes.HasPrefix(rest, vtscan.ResetModes()) || !bytes.Contains(rest, []byte("hello")) {
		t.Fatalf("leaving the crop wrote %q, want ESC[?1049l and a repaint", out)
	}
	out.Reset()
	v.output([]byte("raw\x1b[1m"))
	if got := out.String(); got != "raw\x1b[1m" {
		t.Errorf("after the crop output wrote %q", got)
	}
}

func TestNoScreenMeansANoteNotBytes(t *testing.T) {
	v, out := testView(t, 1000, 1000, 80, 24)
	if v.screenOK() || !v.cropped() {
		t.Fatalf("a 1000×1000 session: screen %v, cropped %v", v.screenOK(), v.cropped())
	}
	v.output([]byte("secret\r\nmore secret\x1b[31m"))
	v.flush()
	got := out.String()
	if strings.Contains(got, "secret") {
		t.Fatalf("the session's bytes reached the window: %q", got)
	}
	if !strings.Contains(got, "\x1b[2J") || !strings.Contains(got, "1000×1000") {
		t.Errorf("no note on a cleared window: %q", got)
	}
}

func TestASessionGrowingPastTheWindowCrops(t *testing.T) {
	v, out := testView(t, 80, 24, 80, 24)
	v.output([]byte("hi"))
	v.resized(100, 30)
	if !v.cropped() || !strings.HasPrefix(out.String(), "hi\x1b[?1049h") {
		t.Fatalf("a session grown past the window wrote %q, cropped %v", out, v.cropped())
	}
	v.output([]byte("\x1b[30;1Hbottom"))
	out.Reset()
	v.flush()
	if !strings.Contains(out.String(), "bottom") {
		t.Errorf("the crop did not follow the resized screen: %q", out)
	}
	// Past what a screen is kept for, the screen goes and the note stays.
	out.Reset()
	v.resized(1000, 1000)
	v.output([]byte("secret"))
	v.flush()
	if v.screenOK() || strings.Contains(out.String(), "secret") || !strings.Contains(out.String(), "1000×1000") {
		t.Errorf("a session grown past a screen: screen %v, wrote %q", v.screenOK(), out)
	}
	// Smaller than the window again: the bytes pass, with nothing to repaint.
	out.Reset()
	v.resized(80, 24)
	v.output([]byte("plain"))
	if v.cropped() || !strings.HasSuffix(out.String(), "plain") {
		t.Errorf("a session that fits again wrote %q, cropped %v", out, v.cropped())
	}
}
