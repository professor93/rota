package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// The reason Claude Code's own refusal leaves behind: the longest STATUS a
// listing has to show, and the one that broke a half-screen terminal.
const clearedReason = "Claude Code cleared the login in its home after a refresh of its own was refused"

// goldenGrid is the listing testdata/grid-full.golden was drawn from, by
// the renderer as it was before tables could fit a terminal: an address too
// long for any narrow column, three windows of usage, and that reason.
func goldenGrid(width int) *grid {
	g := listing(width)
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 3% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "ok")
	g.add("2", "5", "claude", "a.very.long.address.for.work@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	g.add("-", "7", "codex", "b@x", "5h 0%", "90%", "3d ago", "ok (login ends in 2d)")
	return g
}

// ownersGrid is the listing that broke, with addresses of the length people
// have.
func ownersGrid(width int) *grid {
	g := listing(width)
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 3% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "ok")
	g.add("2", "5", "claude", "work.account@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	g.add("-", "7", "codex", "b@x", "5h 0%", "90%", "3d ago", "ok (login ends in 2d)")
	return g
}

// spentGrid is a listing whose five-hour window is spent: "5h 100%" is the
// reading a cut must not turn into another number.
func spentGrid(width int) *grid {
	g := listing(width)
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 100% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "limited")
	g.add("2", "5", "claude", "work.account@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	return g
}

// longAddressGrid has one reading, on the row of an address too long for
// any narrow column.
func longAddressGrid(width int) *grid {
	g := listing(width)
	g.add("1", "3", "claude", "a.very.long.address.for.work@example.com", "5h 3% (4h 12m)", "95%", "just now", "ok")
	g.add("2", "5", "claude", "b@x", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	return g
}

func shortGrid(width int) *grid {
	g := shortListing(width)
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 3%", "just now")
	g.add("2", "5", "claude", "b@x", "-", "2h ago")
	return g
}

func drawn(g *grid) string {
	var b bytes.Buffer
	g.render(&b)
	return b.String()
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// framed checks what fitting promises of every line of a table: none wider
// than the terminal, and all of them as wide as each other, which is what a
// border lining up means. Width is the columns a line takes on screen, so
// a name in Chinese takes two a character.
func framed(t *testing.T, out string, width int) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	frame := screenWidth(lines[0])
	if frame > width {
		t.Fatalf("the frame is %d wide on a %d-column terminal:\n%s", frame, width, out)
	}
	for i, line := range lines {
		if n := screenWidth(line); n != frame {
			t.Fatalf("line %d is %d wide, the frame %d — the borders do not line up:\n%s", i+1, n, frame, out)
		}
	}
	// Every border of every row sits in the same column as the frame's.
	joints := func(s string, marks string) []int {
		var at []int
		r := []rune(s)
		for i, col := 0, 0; i < len(r); {
			if strings.ContainsRune(marks, r[i]) {
				at = append(at, col)
			}
			end, w := char(r, i)
			i, col = end, col+w
		}
		return at
	}
	want := fmt.Sprint(joints(lines[0], "┌┬┐"))
	for i, line := range lines {
		if got := fmt.Sprint(joints(line, "│┌┬┐├┼┤└┴┘")); got != want {
			t.Fatalf("line %d has its borders at %s, the frame at %s:\n%s", i+1, got, want, out)
		}
	}
}

// column is what the column headed name says, all of its lines joined by
// spaces, for the row whose ID is id; "" when the column was left out.
func column(out, name, id string) string {
	var words []string
	at, idAt := -1, -1
	in := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "│") {
			if in {
				break
			}
			continue
		}
		parts := strings.Split(line, "│")
		if idAt < 0 {
			for i, p := range parts {
				switch strings.TrimSpace(p) {
				case name:
					at = i
				case "ID":
					idAt = i
				}
			}
			if at < 0 || idAt < 0 {
				return ""
			}
			continue
		}
		cell := strings.TrimSpace(parts[idAt])
		if cell == id {
			in = true
		} else if cell != "" && in {
			break
		}
		if in {
			if w := strings.TrimSpace(parts[at]); w != "" {
				words = append(words, w)
			}
		}
	}
	return strings.Join(words, " ")
}

// height is how many lines the row whose ID is id takes.
func height(out, id string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		switch {
		case n > 0 && !strings.HasPrefix(line, "│"):
			return n
		case n > 0 && strings.TrimSpace(strings.Split(line, "│")[2]) != "":
			return n
		case n > 0, strings.HasPrefix(line, "│") && strings.TrimSpace(strings.Split(line, "│")[2]) == id:
			n++
		}
	}
	return n
}

// headers is the header line's cells, in order.
func headers(out string) []string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "│") {
			var cells []string
			for _, p := range strings.Split(strings.Trim(line, "│"), "│") {
				cells = append(cells, strings.TrimSpace(p))
			}
			return cells
		}
	}
	return nil
}

// shownAt is, for each column fit drew, the column of g it is.
func shownAt(g *grid, header []string) []int {
	var at []int
	for _, h := range header {
		at = append(at, slices.Index(g.header, h))
	}
	return at
}

// Off a terminal, the table is drawn exactly as it was before tables could
// fit one: byte for byte what the renderer drew then.
func TestTableOffATerminalIsDrawnAsBefore(t *testing.T) {
	if got, want := drawn(goldenGrid(0)), golden(t, "grid-full.golden"); got != want {
		t.Fatalf("the listing changed:\n%s\nwant:\n%s", got, want)
	}
	if got, want := drawn(shortGrid(0)), golden(t, "grid-short.golden"); got != want {
		t.Fatalf("the short listing changed:\n%s\nwant:\n%s", got, want)
	}
}

// A terminal wide enough for the table changes nothing either.
func TestTableOnAWideTerminalIsUnchanged(t *testing.T) {
	if got, want := drawn(goldenGrid(200)), golden(t, "grid-full.golden"); got != want {
		t.Fatalf("200 columns is room enough:\n%s", got)
	}
	if got, want := drawn(shortGrid(80)), golden(t, "grid-short.golden"); got != want {
		t.Fatalf("80 columns is room enough for the short listing:\n%s", got)
	}
}

var (
	fullHeader  = []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}
	reason      = "re-auth needed (" + clearedReason + ")"
	allReadings = "5h 3% (4h 12m) 7d 29% (3d 2h) Fable 48%"
)

// On a terminal narrower than the table, STATUS wraps inside its column at
// its spaces first, then ACCOUNT before its "@"; every column is still
// there, nothing is cut, and every word of the reason is still there.
func TestTableWrapsStatusThenAccount(t *testing.T) {
	for _, width := range []int{120, 100} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := drawn(ownersGrid(width))
			framed(t, out, width)
			if got := fmt.Sprint(headers(out)); got != fmt.Sprint(fullHeader) {
				t.Fatalf("every column is there: %s\n%s", got, out)
			}
			if strings.Contains(out, "…") {
				t.Fatalf("nothing is cut while wrapping can fit it:\n%s", out)
			}
			if got := column(out, "STATUS", "5"); got != reason {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
			if got := column(out, "USAGE", "3"); got != allReadings {
				t.Fatalf("USAGE keeps its readings whole: %q\n%s", got, out)
			}
		})
	}
	// At 120 STATUS alone gives way; ACCOUNT keeps its address on one line.
	if out := drawn(ownersGrid(120)); !strings.Contains(out, "│ work.account@example.com │") {
		t.Fatalf("ACCOUNT gives way only after STATUS:\n%s", out)
	}
	// At 100 ACCOUNT wraps too, before its "@", while STATUS keeps the
	// reason to four lines.
	out := drawn(ownersGrid(100))
	if !strings.Contains(out, "│ work.account │") || !strings.Contains(out, "│ @example.com │") {
		t.Fatalf("at 100 columns the address wraps before its @:\n%s", out)
	}
	if h := height(out, "5"); h > 4 {
		t.Fatalf("the reason takes %d lines:\n%s", h, out)
	}
}

// Once STATUS and ACCOUNT are down to their room, CHECKED and then UNTIL are
// left out whole before either wraps any narrower, and before USAGE loses
// anything. At 80 columns that keeps the readings with their reset times
// and the reason in four lines.
func TestTableLeavesCheckedAndUntilOutBeforeTheTextNarrows(t *testing.T) {
	for _, c := range []struct {
		width int
		shown []string
	}{
		{90, []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "STATUS"}},
		{80, []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "STATUS"}},
	} {
		t.Run(fmt.Sprint(c.width), func(t *testing.T) {
			out := drawn(ownersGrid(c.width))
			framed(t, out, c.width)
			if got := fmt.Sprint(headers(out)); got != fmt.Sprint(c.shown) {
				t.Fatalf("the columns shown: %s, want %s\n%s", got, c.shown, out)
			}
			if strings.Contains(out, "…") {
				t.Fatalf("nothing is cut:\n%s", out)
			}
			if got := column(out, "USAGE", "3"); got != allReadings {
				t.Fatalf("USAGE keeps its readings whole: %q\n%s", got, out)
			}
			if got := column(out, "STATUS", "5"); got != reason {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
			if h := height(out, "5"); h > 4 {
				t.Fatalf("the reason takes %d lines:\n%s", h, out)
			}
			if got := column(out, "ACCOUNT", "5"); got != "work.account @example.com" {
				t.Fatalf("the address, on two lines: %q\n%s", got, out)
			}
		})
	}
}

// CLI is what tells a claude account from a codex one, so it stays until
// STATUS has wrapped on down to ACCOUNT's 16 columns: at 70 it is still
// there, and only a column narrower is it left out, giving STATUS back its
// room.
func TestTableKeepsCLIUntilTheTextIsDownToSixteen(t *testing.T) {
	out := drawn(ownersGrid(70))
	framed(t, out, 70)
	if got := fmt.Sprint(headers(out)); got != "[# ID CLI ACCOUNT USAGE STATUS]" {
		t.Fatalf("at 70 the columns shown: %s\n%s", got, out)
	}
	if column(out, "CLI", "3") != "claude" || column(out, "CLI", "7") != "codex" {
		t.Fatalf("at 70 each row says its CLI:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Fatalf("nothing is cut:\n%s", out)
	}
	if got := column(out, "USAGE", "3"); got != allReadings {
		t.Fatalf("USAGE keeps its readings whole: %q\n%s", got, out)
	}
	if got := column(out, "STATUS", "5"); got != reason {
		t.Fatalf("the reason, read down its column: %q\n%s", got, out)
	}
	header, rows, _ := ownersGrid(70).fit()
	if got := widths(header, rows)[slices.Index(header, "STATUS")]; got > 16 {
		t.Fatalf("STATUS is %d wide with CLI kept, more than 16:\n%s", got, out)
	}

	out = drawn(ownersGrid(69))
	framed(t, out, 69)
	if got := fmt.Sprint(headers(out)); got != "[# ID ACCOUNT USAGE STATUS]" {
		t.Fatalf("at 69 the columns shown: %s\n%s", got, out)
	}
	header, rows, _ = ownersGrid(69).fit()
	if got := widths(header, rows)[slices.Index(header, "STATUS")]; got <= 16 {
		t.Fatalf("with CLI gone STATUS is still only %d wide:\n%s", got, out)
	}
}

// Once nothing is left to leave out, STATUS and ACCOUNT wrap on, down to
// ten columns, and only then is USAGE cut, each of its lines with one "…"
// that keeps its start. The free text is never cut: read down its column,
// it is all still there.
func TestTableCutsUsageOnlyWhenNothingElseCanGive(t *testing.T) {
	for _, width := range []int{60, 50, 45, 40} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := drawn(goldenGrid(width))
			framed(t, out, width)
			if got := fmt.Sprint(headers(out)); got != "[# ID ACCOUNT USAGE STATUS]" {
				t.Fatalf("the columns shown: %s\n%s", got, out)
			}
			if got := column(out, "STATUS", "5"); squeeze(got) != squeeze(reason) {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
			if got := column(out, "ACCOUNT", "5"); squeeze(got) != "a.very.long.address.for.work@example.com" {
				t.Fatalf("the address, read down its column: %q\n%s", got, out)
			}
			usage := column(out, "USAGE", "3")
			if strings.Contains(out, "……") {
				t.Fatalf("one ellipsis per cut:\n%s", out)
			}
			if !strings.HasPrefix(usage, "5h 3") {
				t.Fatalf("a cut reading keeps its start: %q\n%s", usage, out)
			}
			if width >= 60 && usage != allReadings {
				t.Fatalf("at %d columns USAGE is still whole: %q\n%s", width, usage, out)
			}
			if width <= 45 && !strings.Contains(usage, "…") {
				t.Fatalf("at %d columns USAGE is cut: %q\n%s", width, usage, out)
			}
		})
	}
}

// squeeze is text with its spaces taken out: a word broken across the lines
// of a narrow column comes back whole.
func squeeze(s string) string { return strings.ReplaceAll(s, " ", "") }

// At every width from 30 to 200, no header is cut, the id and the place are
// always there, and the reason and the address can be read down their
// columns. The table is no wider than the terminal at every width its
// headers fit in, and at a narrower one is as narrow as they go. Whatever is
// left out is said, in the order it went.
func TestTableNeverCutsAHeader(t *testing.T) {
	for width := 30; width <= 200; width++ {
		for name, g := range map[string]*grid{"full": goldenGrid(width), "owners": ownersGrid(width), "short": shortGrid(width)} {
			out := drawn(g)
			// least is the frame with every column that is never left out
			// down to its header.
			var kept []int
			for i, n := range widths(g.header, nil) {
				if !slices.ContainsFunc(g.drop, func(x leave) bool { return x.col == i }) {
					kept = append(kept, n)
				}
			}
			least := g.frame(kept)
			if width >= least {
				framed(t, out, width)
			} else {
				framed(t, out, least)
				if n := screenWidth(strings.SplitN(out, "\n", 2)[0]); n != least {
					t.Fatalf("%s at %d: too narrow for its headers, drawn %d wide, not %d:\n%s", name, width, n, least, out)
				}
			}
			got := headers(out)
			var gone []string
			for _, x := range g.drop {
				if !slices.Contains(got, g.header[x.col]) {
					gone = append(gone, g.header[x.col])
				}
			}
			if _, _, left := g.fit(); !slices.Equal(left, gone) {
				t.Fatalf("%s at %d: %q left out, %q said\n%s", name, width, gone, left, out)
			}
			for _, h := range got {
				if !slices.Contains(g.header, h) {
					t.Fatalf("%s at %d: a header cut to %q\n%s", name, width, h, out)
				}
			}
			if len(got) < 2 || got[0] != "#" || got[1] != "ID" {
				t.Fatalf("%s at %d: the place and the id are always there: %q\n%s", name, width, got, out)
			}
			if name != "short" {
				if got := column(out, "STATUS", "5"); squeeze(got) != squeeze(reason) {
					t.Fatalf("%s at %d: the reason, read down its column: %q\n%s", name, width, got, out)
				}
			}
			if got := column(out, "ACCOUNT", "3"); squeeze(got) != "manager.ican@gmail.com" {
				t.Fatalf("%s at %d: the address, read down its column: %q\n%s", name, width, got, out)
			}
		}
	}
}

// The short listing at half a screen: ACCOUNT wraps before its "@", CLI is
// left out, and CHECKED — the age of readings --short did not take — stays,
// the last of the short columns to go.
func TestShortTableWrapsTheAddressAtFifty(t *testing.T) {
	out := drawn(shortGrid(50))
	framed(t, out, 50)
	if strings.Contains(out, "…") {
		t.Fatalf("nothing is cut:\n%s", out)
	}
	if got := fmt.Sprint(headers(out)); got != "[# ID ACCOUNT USAGE CHECKED]" {
		t.Fatalf("the columns shown: %s\n%s", got, out)
	}
	if got := column(out, "ACCOUNT", "3"); got != "manager.ican @gmail.com" {
		t.Fatalf("the address, read down its column: %q\n%s", got, out)
	}
}

// A cut that lands on a space, or goes back to one, leaves its column
// narrower than it was given, and what that frees goes on to the next
// column that can use it: at these widths, with USAGE cut, the frame is as
// wide as the terminal.
func TestTableGivesWhatACutLeavesToTheNextColumn(t *testing.T) {
	for _, width := range []int{40, 42, 44, 46, 48, 50} {
		out := drawn(longAddressGrid(width))
		framed(t, out, width)
		if !strings.Contains(out, "…") {
			t.Fatalf("%d columns needs a cut:\n%s", width, out)
		}
		if got := utf8.RuneCountInString(strings.SplitN(out, "\n", 2)[0]); got != width {
			t.Fatalf("a frame %d wide with a cut in it, on a %d-column terminal:\n%s", got, width, out)
		}
	}
	// At any width, what is left over is less than any cut column needs to
	// show more of what it cut.
	for name, mk := range map[string]func(int) *grid{"golden": goldenGrid, "owners": ownersGrid, "spent": spentGrid, "long address": longAddressGrid, "short": shortGrid} {
		for width := 40; width <= 130; width++ {
			g := mk(width)
			header, rows, _ := g.fit()
			got := widths(header, rows)
			left := width - g.frame(got)
			if left <= 0 {
				continue
			}
			for j, i := range shownAt(g, header) {
				col := []string{g.header[i]}
				isCut := false
				for ri, r := range g.rows {
					col = append(col, r[i])
					isCut = isCut || strings.HasSuffix(rows[ri][j], "…") && !strings.HasSuffix(r[i], "…")
				}
				if !isCut {
					continue // not cut, or wrapped
				}
				wider := 0
				for _, c := range col {
					for line := range strings.SplitSeq(cutCell(c, got[j]+left), "\n") {
						wider = max(wider, utf8.RuneCountInString(line))
					}
				}
				if wider > got[j] {
					t.Errorf("%s at %d: %d left over, and %s could have shown more in it:\n%s", name, width, left, g.header[i], drawn(g))
				}
			}
		}
	}
}

// A cut reading ends at a space, never inside a number: on a narrow
// terminal a spent window says "5h 100%…", or failing that "5h…", and never
// "5h 10…", which reads as a different number.
func TestTableCutsAReadingBetweenItsWords(t *testing.T) {
	for _, width := range []int{46, 48, 50} {
		for name, mk := range map[string]func(int) *grid{"spent": spentGrid, "golden": goldenGrid} {
			g := mk(width)
			out := drawn(g)
			framed(t, out, width)
			header, rows, _ := g.fit()
			usage := slices.Index(header, "USAGE")
			for ri, r := range g.rows {
				was := strings.Split(r[4], "\n")
				for k, line := range strings.Split(rows[ri][usage], "\n") {
					kept, isCut := strings.CutSuffix(line, "…")
					if !isCut {
						continue
					}
					if !strings.HasPrefix(was[k], kept+" ") {
						t.Fatalf("%s at %d: %q is %q cut inside a word:\n%s", name, width, line, was[k], out)
					}
				}
			}
			if name == "spent" && !strings.Contains(out, "│ 5h 100%…") {
				t.Fatalf("at %d USAGE is wide enough for the spent window's percentage:\n%s", width, out)
			}
		}
	}
}

func TestCutCellKeepsWholeWords(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"5h 100% (4h 12m)", 16, "5h 100% (4h 12m)"},
		{"5h 100% (4h 12m)", 9, "5h 100%…"},
		{"5h 100% (4h 12m)", 8, "5h 100%…"},
		{"5h 100% (4h 12m)", 7, "5h…"},
		{"5h 100% (4h 12m)", 3, "5h…"},
		{"5h 100% (4h 12m)", 2, "5…"},
		{"5h 3% (4h 12m)", 11, "5h 3% (4h…"},
		{"5h 3% (4h 12m)", 9, "5h 3%…"}, // never "5h 3% (…"
		{"7d 29% (3d 2h)", 7, "7d 29%…"},
		{"7d 29% (3d 2h)", 6, "7d…"},
		{"Fable 48%", 9, "Fable 48%"},
		{"Fable 48%", 8, "Fable…"},
		// No space to go back to, or one that would leave a single letter:
		// cut where n falls.
		{"Fable 48%", 5, "Fabl…"},
		{"a bcdef", 5, "a bc…"},
		// Each line of a cell on its own.
		{"5h 100% (4h 12m)\n7d 29% (3d 2h)", 7, "5h…\n7d 29%…"},
	} {
		if got := cutCell(c.in, c.n); got != c.want {
			t.Errorf("cutCell(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	// What a pipe gets through clip is cut where n falls, as it always was.
	if got := cut("5h 100% (4h 12m)", 6); got != "5h 10…" {
		t.Errorf("cut is not cutCell: %q", got)
	}
}

// A width nothing can fit in still draws a whole table, as narrow as it can,
// with its headers whole.
func TestTableTooNarrowForAnythingStillDrawsWhole(t *testing.T) {
	out := drawn(goldenGrid(10))
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	framed(t, out, utf8.RuneCountInString(lines[0]))
	if got := fmt.Sprint(headers(out)); got != "[# ID ACCOUNT USAGE STATUS]" {
		t.Fatalf("the columns shown: %s\n%s", got, out)
	}
}

func TestWrapLine(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want []string
	}{
		{"fits as it is", 20, []string{"fits as it is"}},
		{"breaks at the spaces it has", 12, []string{"breaks at", "the spaces", "it has"}},
		{"exactly", 7, []string{"exactly"}},
		// An address breaks before its "@", a name after its last ".", "/"
		// or "-" that fits, and only what has none of them where n falls.
		// A word too long for a line starts on the line before it when it
		// breaks within the room left there; when it does not, that line
		// ends where it is rather than take a piece cut where n falls.
		{"a manager.ican@gmail.com b", 10, []string{"a manager.", "ican", "@gmail.com", "b"}},
		{"a manager.ican@gmail.com b", 8, []string{"a", "manager.", "ican", "@gmail.", "com b"}},
		{"x account@example.com", 12, []string{"x account", "@example.com"}},
		{"/usr/local/share/rota/accounts.json", 12, []string{"/usr/local/", "share/rota/", "accounts.", "json"}},
		{"re-auth", 4, []string{"re-", "auth"}},
		{"abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		// A command in backticks is one word, and breaks at its own spaces
		// only when it is longer than the line.
		{"say `rota login --long` now", 20, []string{"say", "`rota login --long`", "now"}},
		{"`rota login --long`", 12, []string{"`rota login", "--long`"}},
		// A quote left open makes the rest of the line one word, which no
		// longer leaves the word before it on a line by itself.
		{"an `unbalanced quote runs on to the end", 20, []string{"an `unbalanced", "quote runs on to", "the end"}},
	} {
		if got := wrapLine(c.in, c.n); !slices.Equal(got, c.want) {
			t.Errorf("wrapLine(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// The notes under the table wrap at their spaces to the terminal, and off a
// terminal are printed exactly as given.
func TestNotesWrapAtWordsOnATerminal(t *testing.T) {
	note := "\na@x: its login ends on 2026-11-09, in 3 days; `rota login`, or /login in one of its windows, before then\n"
	var b bytes.Buffer
	say(&b, 0, note)
	if b.String() != note {
		t.Fatalf("off a terminal, as given: %q", b.String())
	}
	b.Reset()
	say(&b, 40, note)
	got := b.String()
	if strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(note), " ") {
		t.Fatalf("every word kept, in order: %q", got)
	}
	if !strings.HasPrefix(got, "\n") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("the blank line before and the newline after stay: %q", got)
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(note) {
		words[w] = true
	}
	for _, line := range strings.Split(strings.Trim(got, "\n"), "\n") {
		if utf8.RuneCountInString(line) > 40 {
			t.Fatalf("a line wider than the terminal: %q", line)
		}
		for _, w := range strings.Fields(line) {
			if !words[w] {
				t.Fatalf("a word broken mid-way: %q in %q", w, line)
			}
		}
	}
}

// At 80 columns the login note would break inside `rota login`; the command
// moves to the next line whole.
func TestNotesKeepACommandOnOneLine(t *testing.T) {
	note := "\nclaude/manager.ican@gmail.com: its login ends on 2026-11-09, in 3 days; `rota login`, or /login in one of its windows, before then\n"
	if i := strings.Index(note, "`rota login`"); i > 80 || i+len("`rota login`") <= 81 {
		t.Fatalf("the note no longer puts the command across column 80 (%d)", i)
	}
	var b bytes.Buffer
	say(&b, 80, note)
	got := b.String()
	if !strings.Contains(got, "\n`rota login`, or") {
		t.Fatalf("the command starts a line, whole:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if utf8.RuneCountInString(line) > 80 {
			t.Fatalf("a line wider than the terminal: %q", line)
		}
	}
}

// The listing itself, on a terminal of 90 columns: the dead account's reason
// wraps in STATUS, the frame holds, and the notes under the table wrap too.
// Off a terminal the same store lists exactly as it always has.
func TestListFitsTheTerminalItIsOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	now := time.Now()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"manager.ican@gmail.com","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]},
		 "extra":{"refresh_token_expires_at":"%[2]d"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "dead":true,"deadReason":%[3]q}]}`,
		now.UnixMilli(), now.Add(2*24*time.Hour+time.Hour).UnixMilli(), clearedReason))

	piped, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, piped)
	}

	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb, in: strings.NewReader(""), cols: 90}
	if err := c.list(nil); err != nil {
		t.Fatal(err)
	}
	table, notes, ok := strings.Cut(out.String(), "┘\n")
	if !ok {
		t.Fatalf("a table:\n%s", out.String())
	}
	framed(t, table+"┘\n", 90)
	if got := column(table, "STATUS", "2"); got != reason {
		t.Fatalf("the reason, read down its column: %q\n%s", got, table)
	}
	for _, line := range strings.Split(notes, "\n") {
		if utf8.RuneCountInString(line) > 90 {
			t.Fatalf("a note wider than the terminal: %q\n%s", line, notes)
		}
	}
	if !strings.Contains(notes, "rota login 1 before then") {
		t.Fatalf("the login note is there:\n%s", notes)
	}
	if strings.Join(strings.Fields(notes), " ") != strings.Join(strings.Fields(piped[strings.Index(piped, "┘\n")+len("┘\n"):]), " ") {
		t.Fatalf("the notes say what they said off a terminal:\n%s\n--\n%s", notes, piped)
	}
	if utf8.RuneCountInString(strings.SplitN(piped, "\n", 2)[0]) <= 90 {
		t.Fatalf("off a terminal the table keeps its natural width:\n%s", piped)
	}
}

// A listing that leaves columns out to fit the terminal says so, in one note
// right under the table naming each of them in the order they went, and one
// that leaves nothing out says nothing of it: at every width from 30 to 200,
// for both listings. Off a terminal, nothing is left out and nothing said.
func TestListSaysWhatItLeftOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	now := time.Now()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":4,"accounts":[
		{"id":1,"provider":"claude","email":"manager.ican@gmail.com","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]}},
		{"id":2,"provider":"claude","email":"work.account@example.com","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "dead":true,"deadReason":%[2]q},
		{"id":3,"provider":"codex","email":"b@x","token":{"accessToken":"t"},"quotaAt":%[1]d}]}`,
		now.UnixMilli(), clearedReason))

	notes := map[string]string{
		"[CHECKED]":           "CHECKED is left out to fit this terminal; a wider one or `rota list --json` shows it.",
		"[CHECKED UNTIL]":     "CHECKED and UNTIL are left out to fit this terminal; a wider one or `rota list --json` shows them.",
		"[CHECKED UNTIL CLI]": "CHECKED, UNTIL and CLI are left out to fit this terminal; a wider one or `rota list --json` shows them.",
		"[CLI]":               "CLI is left out to fit this terminal; a wider one or `rota list --json` shows it.",
		"[CLI CHECKED]":       "CLI and CHECKED are left out to fit this terminal; a wider one or `rota list --json` shows them.",
	}
	seen := map[string]bool{}
	for _, args := range [][]string{nil, {"-s"}} {
		piped, _, code := call(t, append([]string{"list"}, args...)...)
		if code != 0 || strings.Contains(piped, "left out") {
			t.Fatalf("off a terminal nothing is left out or said (%d):\n%s", code, piped)
		}
		all := headers(piped)
		order := []string{"CHECKED", "UNTIL", "CLI"}
		if args != nil {
			order = []string{"CLI", "CHECKED"}
		}
		for width := 30; width <= 200; width++ {
			var out, errb bytes.Buffer
			c := &cli{out: &out, err: &errb, in: strings.NewReader(""), cols: width}
			if err := c.list(args); err != nil {
				t.Fatal(err)
			}
			table, after, _ := strings.Cut(out.String(), "┘\n")
			shown := headers(table)
			var gone []string
			for _, h := range order {
				if !slices.Contains(shown, h) {
					gone = append(gone, h)
				}
			}
			if len(shown)+len(gone) != len(all) {
				t.Fatalf("%v at %d: %q shown of %q, beyond the short columns:\n%s", args, width, shown, all, out.String())
			}
			said := strings.Count(strings.Join(strings.Fields(after), " "), "left out")
			if len(gone) == 0 {
				if said != 0 {
					t.Fatalf("%v at %d: nothing left out, and a note says something was:\n%s", args, width, out.String())
				}
				continue
			}
			want, ok := notes[fmt.Sprint(gone)]
			if !ok {
				t.Fatalf("%v at %d: %q left out, a combination with no note here:\n%s", args, width, gone, out.String())
			}
			seen[fmt.Sprint(gone)] = true
			first, _, _ := strings.Cut(strings.TrimPrefix(after, "\n"), "\n\n")
			if said != 1 || strings.Join(strings.Fields(first), " ") != want {
				t.Fatalf("%v at %d: %q left out, and the note under the table is %q:\n%s", args, width, gone, first, out.String())
			}
			for _, line := range strings.Split(first, "\n") {
				if screenWidth(line) > width {
					t.Fatalf("%v at %d: the note is wider than the terminal: %q", args, width, line)
				}
			}
		}
	}
	for k := range notes {
		if !seen[k] {
			t.Errorf("no width left out %s", k)
		}
	}
}
