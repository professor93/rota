package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
)

// longHeld is held sessions with what makes a row too wide for half a
// screen: a long title, a long address, and folders deep in a home
// directory. One has stopped, and one is a shell, with no account.
func longHeld() []rotaclient.Session {
	ended, exit := time.Now(), -1
	return []rotaclient.Session{
		{ID: "a1b2c3d4e5f60718", Name: "api", Status: "running", State: "live", Account: 1,
			AccountLabel: "manager.ican@gmail.com", Provider: "claude",
			Cwd: "/Users/someone/GolandProjects/a-rather-long-project-name/services/api"},
		{ID: "0f0e0d0c0b0a0908", Name: "Review of the account rotation when a refresh token is reused",
			Status: "stopped", State: "interrupted", Ended: &ended, Exit: &exit, Account: 2,
			AccountLabel: "a.very.long.address.for.work@example.com", Provider: "codex",
			Cwd: "/Users/someone/work/clients/northwind/infrastructure/terraform"},
		{ID: "cafe0d0c0b0a0908", Name: "shell", Status: "running", State: "live", Cwd: "/tmp"},
	}
}

// wideHeld is longHeld with a title and an account in Japanese, a folder in
// Chinese, which take two columns a character, and a shell named with
// emoji: a flag and a joined pair.
func wideHeld() []rotaclient.Session {
	held := longHeld()
	held[1].Name = "アカウントの切り替えでリフレッシュトークンが再利用されたときの確認"
	held[1].AccountLabel = "仕事用アカウント@example.com"
	held[1].Cwd = "/Users/someone/项目/客户端/服务/接口/测试"
	held[2].Name = "🇯🇵 launch 👩\u200d💻"
	return held
}

// heldAt is the held sessions as rota list --sessions draws them on a
// terminal width columns wide, 0 being no terminal at all.
func heldAt(held []rotaclient.Session, width int) string {
	var out bytes.Buffer
	c := &cli{out: &out, err: &out, cols: width}
	c.showHeld(held, width)
	return out.String()
}

// heldRows is the rows of a held sessions listing, a line each.
func heldRows(out string) []string {
	_, rest, _ := strings.Cut(out, "Held sessions:\n")
	return rows(rest)
}

// Off a terminal, and on one wide enough, the held sessions are what
// text/tabwriter drew before the listing could fit one, byte for byte.
func TestHeldOffATerminalAreListedAsBefore(t *testing.T) {
	var want bytes.Buffer
	fmt.Fprintln(&want, "\nHeld sessions:")
	w := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
	for _, h := range longHeld() {
		dot, what := "●", h.Status
		if h.Ended != nil {
			dot, what = "○", h.State
		}
		who := "-"
		if h.Account != 0 {
			who = fmt.Sprintf("#%d %s", h.Account, h.AccountLabel)
		}
		fmt.Fprintf(w, "  %s %s\t%s\t%s\t%s\t%s\t%s\n", dot, clip(h.Name, 44), who, orDash(h.Provider), what, orDash(h.Cwd), short(h.ID))
	}
	_ = w.Flush()
	for _, width := range []int{0, 200} {
		if got := heldAt(longHeld(), width); got != want.String() {
			t.Fatalf("at %d the listing changed:\n%s\nwant:\n%s", width, got, want.String())
		}
	}
}

// Off a terminal a name in Japanese and a folder in Chinese are padded by
// the columns they take on screen, so every column after them lines up.
func TestHeldLineUpWideCharactersOffATerminal(t *testing.T) {
	out := heldAt(wideHeld(), 0)
	if want := golden(t, "held-plain.golden"); out != want {
		t.Fatalf("the listing:\n%s\nwant:\n%s", out, want)
	}
	lines := heldRows(out)
	for _, r := range lines {
		if fmt.Sprint(starts(r)) != fmt.Sprint(starts(lines[0])) {
			t.Fatalf("columns at %v, the first row's at %v:\n%s", starts(r), starts(lines[0]), out)
		}
	}
}

// On a narrow terminal every line fits, every row stays one line with its
// columns lined up, and what is cut is the free text first: a title keeps
// its start and a folder its end, each kept eight columns wide until the
// account has given way as far, and the start of the id is whole at every
// width.
func TestHeldFitANarrowTerminal(t *testing.T) {
	for name, held := range map[string][]rotaclient.Session{"ascii": longHeld(), "wide": wideHeld()} {
		for _, width := range []int{120, 80, 60} {
			t.Run(fmt.Sprint(name, width), func(t *testing.T) {
				out := heldAt(held, width)
				cellsWhole(t, out)
				for _, line := range strings.Split(out, "\n") {
					if n := screenWidth(line); n > width {
						t.Fatalf("a line %d wide at %d columns: %q\n%s", n, width, line, out)
					}
				}
				lines := heldRows(out)
				if len(lines) != len(held) {
					t.Fatalf("one line a row: %d rows, %d lines\n%s", len(held), len(lines), out)
				}
				for i, r := range lines {
					if fmt.Sprint(starts(r)) != fmt.Sprint(starts(lines[0])) {
						t.Fatalf("columns at %v, the first row's at %v:\n%s", starts(r), starts(lines[0]), out)
					}
					cells := rowCells(r)
					if len(cells) != 6 || cells[5] != short(held[i].ID) {
						t.Fatalf("the id %s, whole, last in its row: %q\n%s", short(held[i].ID), r, out)
					}
					title := strings.TrimPrefix(strings.TrimPrefix(cells[0], "● "), "○ ")
					if kept, cut := strings.CutSuffix(title, "…"); !strings.HasPrefix(held[i].Name, kept) || !cut && title != held[i].Name {
						t.Fatalf("the title keeps its start: %q\n%s", cells[0], out)
					}
					if kept, cut := strings.CutPrefix(cells[4], "…"); !strings.HasSuffix(held[i].Cwd, kept) || !cut && kept != held[i].Cwd {
						t.Fatalf("the folder keeps its end: %q\n%s", cells[4], out)
					}
					// The free text keeps eight columns until the account
					// has given way as far.
					if who := cells[1]; screenWidth(who) > cutSoft {
						if least := heldName(cells[0][:strings.Index(cells[0], " ")+1]+held[i].Name, heldMark+cutSoft); screenWidth(cells[0]) < screenWidth(least) {
							t.Fatalf("the title cut to %q while the account is %q:\n%s", cells[0], who, out)
						}
						if screenWidth(cells[4]) < min(cutSoft, screenWidth(held[i].Cwd)) {
							t.Fatalf("the folder cut to %q while the account is %q:\n%s", cells[4], who, out)
						}
					}
				}
				// 120 columns cuts only the free text, and 80 leaves the
				// shorter account whole.
				if width >= 80 && !strings.Contains(out, "  #1 manager.ican@gmail.com  ") {
					t.Fatalf("at %d the shorter account is whole:\n%s", width, out)
				}
				if width >= 120 && !strings.Contains(out, "  #2 "+held[1].AccountLabel+"  ") {
					t.Fatalf("at %d the longest account is whole:\n%s", width, out)
				}
			})
		}
	}
}

// From no terminal to 220 columns nothing panics, every row is one line,
// no character is halved and the id is whole, and from the narrowest width
// the rows can be drawn at, no line is wider than the terminal and the
// cells line up.
func TestHeldAtEveryWidth(t *testing.T) {
	for name, held := range map[string][]rotaclient.Session{"ascii": longHeld(), "wide": wideHeld()} {
		t.Run(name, func(t *testing.T) {
			floor := 0
			for _, l := range heldRows(heldAt(held, 1)) {
				floor = max(floor, screenWidth(l))
			}
			if floor > 34 {
				t.Fatalf("the rows are %d columns at their narrowest:\n%s", floor, heldAt(held, 1))
			}
			for width := 0; width <= 220; width++ {
				out := heldAt(held, width)
				cellsWhole(t, out)
				lines := heldRows(out)
				if len(lines) != len(held) {
					t.Fatalf("one line a row at %d:\n%s", width, out)
				}
				for i, r := range lines {
					if !strings.HasSuffix(r, "  "+short(held[i].ID)) {
						t.Fatalf("the id %s, whole, at %d:\n%s", short(held[i].ID), width, out)
					}
					if width >= floor && (screenWidth(r) > width || fmt.Sprint(starts(r)) != fmt.Sprint(starts(lines[0]))) {
						t.Fatalf("at %d:\n%s", width, out)
					}
				}
			}
		})
	}
}

func TestHeldName(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"● api", 10, "● api"},
		{"○ Review of the account", 10, "○ Review…"},
		{"○ Review of the account", 4, "○ R…"},
		// The dot keeps its space when the first character is too wide
		// for what is left.
		{"○ アカウント", 4, "○ …"},
		{"○ アカウント", 5, "○ ア…"},
		{"● 🇯🇵 launch", 5, "● 🇯🇵…"},
	} {
		if got := heldName(c.in, c.n); got != c.want {
			t.Errorf("heldName(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
