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
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width, give: []int{7, 3}, keep: []int{6, 5, 2, 1, 0}}
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 3% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "ok")
	g.add("2", "5", "claude", "a.very.long.address.for.work@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	g.add("-", "7", "codex", "b@x", "5h 0%", "90%", "3d ago", "ok (login ends in 2d)")
	return g
}

// ownersGrid is the listing that broke, with addresses of the length people
// have.
func ownersGrid(width int) *grid {
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width, give: []int{7, 3}, keep: []int{6, 5, 2, 1, 0}}
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 3% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "ok")
	g.add("2", "5", "claude", "work.account@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	g.add("-", "7", "codex", "b@x", "5h 0%", "90%", "3d ago", "ok (login ends in 2d)")
	return g
}

// spentGrid is a listing whose five-hour window is spent: "5h 100%" is the
// reading a cut must not turn into another number.
func spentGrid(width int) *grid {
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width, give: []int{7, 3}, keep: []int{6, 5, 2, 1, 0}}
	g.add("1", "3", "claude", "manager.ican@gmail.com", "5h 100% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "limited")
	g.add("2", "5", "claude", "work.account@example.com", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	return g
}

// longAddressGrid has one reading, on the row of an address too long for
// any narrow column.
func longAddressGrid(width int) *grid {
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width, give: []int{7, 3}, keep: []int{6, 5, 2, 1, 0}}
	g.add("1", "3", "claude", "a.very.long.address.for.work@example.com", "5h 3% (4h 12m)", "95%", "just now", "ok")
	g.add("2", "5", "claude", "b@x", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	return g
}

func shortGrid(width int) *grid {
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "CHECKED"}, width: width, give: []int{3}, keep: []int{5, 2, 1, 0}}
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
		col := 0
		for _, r := range s {
			if strings.ContainsRune(marks, r) {
				at = append(at, col)
			}
			col += runeWidth(r)
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

// column is what a column says, all of its lines joined by spaces, for the
// row whose ID is id.
func column(out string, col int, id string) string {
	var words []string
	in := false
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "│") {
			if in {
				break
			}
			continue
		}
		parts := strings.Split(line, "│")
		cell := strings.TrimSpace(parts[2])
		if cell == id {
			in = true
		} else if cell != "" && in {
			break
		}
		if in {
			if w := strings.TrimSpace(parts[col+1]); w != "" {
				words = append(words, w)
			}
		}
	}
	return strings.Join(words, " ")
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

// On a terminal narrower than the table, STATUS wraps inside its column at
// its spaces, and every word of the reason is still there.
func TestTableWrapsStatusToFitATerminal(t *testing.T) {
	for _, width := range []int{120, 90} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := drawn(ownersGrid(width))
			framed(t, out, width)
			if got := column(out, 7, "5"); got != "re-auth needed ("+clearedReason+")" {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
			if strings.Contains(out, "…") {
				t.Fatalf("nothing is cut while wrapping can fit it:\n%s", out)
			}
			for _, fixed := range []string{"│ claude │", "│ just now │", "│ 95%   │"} {
				if !strings.Contains(out, fixed) {
					t.Fatalf("the short columns keep their width (%q):\n%s", fixed, out)
				}
			}
			if !strings.Contains(out, "│ 5h 3% (4h 12m) │") {
				t.Fatalf("USAGE keeps its lines:\n%s", out)
			}
		})
	}
	// At 120 STATUS alone gives way; ACCOUNT keeps its address on one line.
	if out := drawn(ownersGrid(120)); !strings.Contains(out, "│ work.account@example.com │") {
		t.Fatalf("ACCOUNT gives way only after STATUS:\n%s", out)
	}
}

// Once STATUS is as narrow as it goes, ACCOUNT wraps next, and an address
// too long for the column is broken where the column ends.
func TestTableWrapsAccountAfterStatus(t *testing.T) {
	out := drawn(ownersGrid(90))
	if strings.Contains(out, "│ work.account@example.com │") {
		t.Fatalf("at 90 columns ACCOUNT gives way too:\n%s", out)
	}
	if got := strings.ReplaceAll(column(out, 3, "5"), " ", ""); got != "work.account@example.com" {
		t.Fatalf("the address, read down its column: %q\n%s", got, out)
	}
	if got := column(out, 7, "5"); got != "re-auth needed ("+clearedReason+")" {
		t.Fatalf("the reason, read down its column: %q\n%s", got, out)
	}
}

// A terminal too narrow for even that cuts the widest cells, each cut with
// one "…", and keeps the start of what it cuts.
func TestTableCutsOnlyWhenWrappingCannotFit(t *testing.T) {
	for _, width := range []int{80, 60, 40} {
		out := drawn(goldenGrid(width))
		framed(t, out, width)
		if !strings.Contains(out, "…") {
			t.Fatalf("%d columns needs a cut:\n%s", width, out)
		}
		if strings.Contains(out, "……") {
			t.Fatalf("one ellipsis per cut:\n%s", out)
		}
		got := squeeze(strings.TrimSuffix(column(out, 7, "5"), "…"))
		if got == "" || !strings.HasPrefix(squeeze("re-auth needed ("+clearedReason+")"), got) {
			t.Fatalf("the reason keeps its start at %d: %q\n%s", width, got, out)
		}
	}
}

// squeeze is text with its spaces taken out: a word broken across the lines
// of a narrow column comes back whole.
func squeeze(s string) string { return strings.ReplaceAll(s, " ", "") }

// Cutting starts with the other columns: USAGE first, then the short fixed
// ones. STATUS and ACCOUNT go on wrapping below their usual minimum, and at
// 75 and 70 columns the whole reason is still there.
func TestTableCutsTheOtherColumnsBeforeTheReason(t *testing.T) {
	for _, width := range []int{76, 75, 70} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := drawn(ownersGrid(width))
			framed(t, out, width)
			if got := column(out, 7, "5"); squeeze(got) != squeeze("re-auth needed ("+clearedReason+")") {
				t.Fatalf("the reason, read down its column: %q\n%s", got, out)
			}
			if got := column(out, 3, "5"); squeeze(got) != "work.account@example.com" {
				t.Fatalf("the address, read down its column: %q\n%s", got, out)
			}
			for _, fixed := range []string{"│ claude │", "│ just now │", "│ 95%   │"} {
				if !strings.Contains(out, fixed) {
					t.Fatalf("the short fixed columns are cut last (%q):\n%s", fixed, out)
				}
			}
		})
	}
	if out := drawn(ownersGrid(70)); !strings.Contains(out, "│ 5h 3%…") {
		t.Fatalf("USAGE is the first column cut, and keeps its percentage:\n%s", out)
	}
}

// The short listing at half a screen: ACCOUNT wraps rather than being cut,
// and the whole address reassembles from its lines.
func TestShortTableWrapsTheAddressAtFifty(t *testing.T) {
	out := drawn(shortGrid(50))
	framed(t, out, 50)
	if strings.Contains(out, "…") {
		t.Fatalf("nothing is cut:\n%s", out)
	}
	if got := column(out, 3, "3"); squeeze(got) != "manager.ican@gmail.com" {
		t.Fatalf("the address, read down its column: %q\n%s", got, out)
	}
}

// A cut that lands on a space, or goes back to one, leaves its column
// narrower than it was given, and what that frees goes on to the next
// column that can use it. At these widths the frame used to come out a
// column short with USAGE still cut; now it is as wide as the terminal.
func TestTableGivesWhatACutLeavesToTheNextColumn(t *testing.T) {
	for _, width := range []int{72, 73, 74, 77, 78} {
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
			header, rows := g.fit()
			got := widths(header, rows)
			left := width - frame(got)
			if left <= 0 {
				continue
			}
			for i := range got {
				col := []string{g.header[i]}
				isCut := false
				for ri, r := range g.rows {
					col = append(col, r[i])
					isCut = isCut || strings.HasSuffix(rows[ri][i], "…") && !strings.HasSuffix(r[i], "…")
				}
				if !isCut {
					continue // not cut, or wrapped
				}
				wider := 0
				for _, c := range col {
					for line := range strings.SplitSeq(cutCell(c, got[i]+left), "\n") {
						wider = max(wider, utf8.RuneCountInString(line))
					}
				}
				if wider > got[i] {
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
	for _, width := range []int{66, 70, 72} {
		for name, mk := range map[string]func(int) *grid{"spent": spentGrid, "golden": goldenGrid} {
			g := mk(width)
			out := drawn(g)
			framed(t, out, width)
			_, rows := g.fit()
			for ri, r := range g.rows {
				was := strings.Split(r[4], "\n")
				for k, line := range strings.Split(rows[ri][4], "\n") {
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

// A width nothing can fit in still draws a whole table, as narrow as it can.
func TestTableTooNarrowForAnythingStillDrawsWhole(t *testing.T) {
	out := drawn(goldenGrid(10))
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	framed(t, out, utf8.RuneCountInString(lines[0]))
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
		now.UnixMilli(), now.Add(3*24*time.Hour+time.Hour).UnixMilli(), clearedReason))

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
	if got := column(table, 7, "2"); got != "re-auth needed ("+clearedReason+")" {
		t.Fatalf("the reason, read down its column: %q\n%s", got, table)
	}
	for _, line := range strings.Split(notes, "\n") {
		if utf8.RuneCountInString(line) > 90 {
			t.Fatalf("a note wider than the terminal: %q\n%s", line, notes)
		}
	}
	if !strings.Contains(notes, "/login in one of its") {
		t.Fatalf("the login note is there:\n%s", notes)
	}
	if strings.Join(strings.Fields(notes), " ") != strings.Join(strings.Fields(piped[strings.Index(piped, "┘\n")+len("┘\n"):]), " ") {
		t.Fatalf("the notes say what they said off a terminal:\n%s\n--\n%s", notes, piped)
	}
	if utf8.RuneCountInString(strings.SplitN(piped, "\n", 2)[0]) <= 90 {
		t.Fatalf("off a terminal the table keeps its natural width:\n%s", piped)
	}
}
