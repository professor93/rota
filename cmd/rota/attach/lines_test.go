package attach

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/professor93/rota/cmd/rota/client"
)

// TestTheLinesSayWhyAndHowBack: the one line an attach ends with says why it
// ended and, where there is one, how to come back, in fixed words.
func TestTheLinesSayWhyAndHowBack(t *testing.T) {
	api := client.Session{ID: "4f2a9c1e", Name: "api"}
	spaced := client.Session{ID: "4f2a9c1e", Name: "my api"}
	unnamed := client.Session{ID: "4f2a9c1e"}
	dirty := client.Session{ID: "4f2a9c1e", Name: "a\x1b[2Jb"}
	custom := Options{Again: func(w string) string { return "attach " + w }}
	for _, c := range []struct {
		what string
		r    Result
		o    Options
		want string
	}{
		{"detached", Result{Why: Detached, Session: api}, Options{},
			"rota: detached from api; rota run --attach api returns to it"},
		{"detached, quoted", Result{Why: Detached, Session: spaced}, Options{},
			"rota: detached from my api; rota run --attach 'my api' returns to it"},
		{"detached, no name", Result{Why: Detached, Session: unnamed}, Options{},
			"rota: detached from 4f2a9c1e; rota run --attach 4f2a9c1e returns to it"},
		{"detached, a dirty name", Result{Why: Detached, Session: dirty}, Options{},
			"rota: detached from a [2Jb; rota run --attach 4f2a9c1e returns to it"},
		{"detached, custom again", Result{Why: Detached, Session: api}, custom,
			"rota: detached from api; attach api returns to it"},
		{"ended", Result{Why: Ended, Session: api, Code: 3}, Options{},
			"rota: api ended, exit 3"},
		{"ended 0", Result{Why: Ended, Session: api, Code: 0}, Options{},
			"rota: api ended, exit 0"},
		{"ended -1", Result{Why: Ended, Session: api, Code: -1}, Options{},
			"rota: api was interrupted: its holder went without its CLI's exit"},
		{"lost", Result{Why: Lost, Session: api, Err: errors.New("the daemon\nwent away")}, Options{},
			"rota: lost api: the daemon went away; rota run --attach api tries again"},
		{"lost, custom again", Result{Why: Lost, Session: api, Err: errors.New("gone")}, custom,
			"rota: lost api: gone; attach api tries again"},
		{"refused", Result{Why: Refused, Session: api, Err: errors.New("no such session")}, Options{},
			"rota: no such session"},
	} {
		if got := c.r.Line(c.o); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.what, got, c.want)
		}
	}
}

// TestExitCodes follows the plan's Ruling 13: 0 for a detach, the CLI's own
// code clamped to 0–255 with an interruption as 1, and 1 for a lost or
// refused attach.
func TestExitCodes(t *testing.T) {
	for _, c := range []struct {
		r    Result
		want int
	}{
		{Result{Why: Detached}, 0},
		{Result{Why: Ended, Code: 0}, 0},
		{Result{Why: Ended, Code: 3}, 3},
		{Result{Why: Ended, Code: -1}, 1},
		{Result{Why: Ended, Code: 300}, 255},
		{Result{Why: Lost}, 1},
		{Result{Why: Refused}, 1},
		{Result{}, 1},
	} {
		if got := c.r.ExitCode(); got != c.want {
			t.Errorf("%+v: ExitCode() = %d, want %d", c.r, got, c.want)
		}
	}
}

// TestTheCopyIsFixed: the hint row, the help card and the crop note say the
// plan's words, with the chord key named.
func TestTheCopyIsFixed(t *testing.T) {
	if got, want := hintRow(0x1c, 200), `Ctrl-\  d leave · n/p next/prev · k keyboard · z fit · ? help · Ctrl-\ itself`; got != want {
		t.Errorf("hintRow:\n got %q\nwant %q", got, want)
	}
	if got, want := hintRow(0x1d, 200), "Ctrl-]  d leave · n/p next/prev · k keyboard · z fit · ? help · Ctrl-] itself"; got != want {
		t.Errorf("hintRow(Ctrl-]):\n got %q\nwant %q", got, want)
	}
	card := []string{
		`rota: to leave, press Ctrl-\ then d; the session goes on`,
		`rota: after Ctrl-\, press`,
		`  d        leave; the session goes on`,
		`  n  p     next, previous session`,
		`  k        take the keyboard`,
		`  z        fit the session to this window`,
		`  ?        these keys`,
		`  Ctrl-\   send Ctrl-\ itself`,
	}
	if got := helpCard(0x1c, true); strings.Join(got, "\n") != strings.Join(card, "\n") {
		t.Errorf("helpCard(teach):\n got %q\nwant %q", got, card)
	}
	if got := helpCard(0x1c, false); strings.Join(got, "\n") != strings.Join(card[1:], "\n") {
		t.Errorf("helpCard:\n got %q\nwant %q", got, card[1:])
	}
	if got, want := cropNote("api", 0x1c, 100, 30, 40, 12), `rota: api is 100×30, this window 40×12; Ctrl-\ z fits it here`; got != want {
		t.Errorf("cropNote:\n got %q\nwant %q", got, want)
	}
	if got, want := cropNote("a\x1bb", 0x1c, 100, 30, 40, 12), `rota: a b is 100×30, this window 40×12; Ctrl-\ z fits it here`; got != want {
		t.Errorf("cropNote of a dirty name:\n got %q\nwant %q", got, want)
	}
}

// TestAHintRowKeepsWholeItems: a narrow window drops the items that do not
// fit rather than cutting one in half.
func TestAHintRowKeepsWholeItems(t *testing.T) {
	if got, want := hintRow(0x1c, 31), `Ctrl-\  d leave · n/p next/prev`; got != want {
		t.Errorf("hintRow at 31:\n got %q\nwant %q", got, want)
	}
	if got, want := hintRow(0x1c, 30), `Ctrl-\  d leave`; got != want {
		t.Errorf("hintRow at 30:\n got %q\nwant %q", got, want)
	}
	if got, want := hintRow(0x1c, 4), `Ctrl`; got != want {
		t.Errorf("hintRow at 4: got %q, want %q", got, want)
	}
	for cols := 0; cols <= 90; cols++ {
		if w := ansi.StringWidth(hintRow(0x1c, cols)); w > cols {
			t.Errorf("hintRow at %d is %d cells", cols, w)
		}
	}
}

// cup is a cursor position, CSI row;col H.
var cup = regexp.MustCompile(`\x1b\[(\d+);(\d+)H`)

// rowsOf splits what a draw wrote at each cursor position into its row,
// column and the printable text written there.
func rowsOf(t *testing.T, b []byte) (rows, cols []int, texts []string) {
	t.Helper()
	s := string(b)
	at := cup.FindAllStringSubmatchIndex(s, -1)
	for i, m := range at {
		end := len(s)
		if i+1 < len(at) {
			end = at[i+1][0]
		}
		r, _ := strconv.Atoi(s[m[2]:m[3]])
		c, _ := strconv.Atoi(s[m[4]:m[5]])
		rows = append(rows, r)
		cols = append(cols, c)
		texts = append(texts, ansi.Strip(s[m[1]:end]))
	}
	return rows, cols, texts
}

// TestDrawnRowsFitTheWindow: an overlay never writes past the window, so
// the person's terminal never wraps or scrolls under it.
func TestDrawnRowsFitTheWindow(t *testing.T) {
	row := drawRow(24, hintRow(0x1c, 20), 20)
	const head, tail = "\x1b7\x1b[24;1H\x1b[7m", "\x1b[0m\x1b8"
	if !strings.HasPrefix(string(row), head) || !strings.HasSuffix(string(row), tail) {
		t.Fatalf("drawRow = %q, want it between %q and %q", row, head, tail)
	}
	mid := string(row)[len(head) : len(row)-len(tail)]
	if strings.ContainsRune(mid, 0x1b) {
		t.Errorf("drawRow's text holds an escape: %q", mid)
	}
	if w := ansi.StringWidth(mid); w != 20 {
		t.Errorf("drawRow wrote %d cells, want 20: %q", w, mid)
	}
	if w := ansi.StringWidth(ansi.Strip(string(drawRow(1, "hi", 20)))); w != 20 {
		t.Errorf("a short text is not padded to the window: %d cells", w)
	}
	if mid := string(drawRow(3, "a\x1b[31mb", 10)); !strings.Contains(mid, "a [31mb") {
		t.Errorf("drawRow kept an escape from its text: %q", mid)
	}

	for _, size := range [][2]int{{30, 8}, {80, 24}, {12, 5}, {200, 60}} {
		cols, rows := size[0], size[1]
		b := drawCard(helpCard(0x1c, true), cols, rows)
		if !strings.HasPrefix(string(b), "\x1b7") || !strings.HasSuffix(string(b), "\x1b8") {
			t.Errorf("%dx%d: drawCard does not save and restore the cursor: %q", cols, rows, b)
		}
		rs, cs, texts := rowsOf(t, b)
		if len(rs) == 0 {
			t.Errorf("%dx%d: drawCard drew nothing", cols, rows)
		}
		for i := range rs {
			w := ansi.StringWidth(texts[i])
			if rs[i] < 1 || rs[i] > rows || cs[i] < 1 || cs[i]-1+w > cols {
				t.Errorf("%dx%d: row %d at column %d is %d cells: %q", cols, rows, rs[i], cs[i], w, texts[i])
			}
			if w > cols {
				t.Errorf("%dx%d: a row is %d cells", cols, rows, w)
			}
		}
	}
	if b := drawCard(helpCard(0x1c, false), 80, 24); !strings.Contains(string(b), "these keys") {
		t.Errorf("a card with room cut its lines: %q", b)
	}
	if b := drawCard([]string{"x"}, 2, 2); len(b) != 0 {
		t.Errorf("a window too small for a border drew %q", b)
	}
}
