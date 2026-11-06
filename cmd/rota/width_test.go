package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

func TestRuneWidth(t *testing.T) {
	for _, c := range []struct {
		r    rune
		want int
	}{
		// One column: ASCII, Latin, the frame, its "…", and the symbols
		// that are text unless asked to be pictures.
		{'a', 1}, {'~', 1}, {'é', 1}, {'Ω', 1}, {'ж', 1}, {'—', 1},
		{'…', 1}, {'─', 1}, {'│', 1}, {'┼', 1},
		{'☀', 1}, {'✓', 1}, {'★', 1}, {'❤', 1}, {'ｱ', 1}, // halfwidth katakana
		// None: marks on the letter before, and what is never drawn.
		{'\u0301', 0}, {'\u20dd', 0}, {'\u3099', 0}, {'\ufe0f', 0}, {'\ufe0e', 0},
		{'\u200b', 0}, {'\u200d', 0}, {'\u200f', 0}, {'\u2060', 0}, {'\ufeff', 0},
		{'\u1160', 0}, {'\U000e0067', 0},
		// Two: Chinese, Japanese, Korean, fullwidth forms, emoji.
		{'仕', 2}, {'工', 2}, {'あ', 2}, {'ア', 2}, {'ー', 2}, {'한', 2}, {'ᄀ', 2},
		{'、', 2}, {'。', 2}, {'　', 2}, {'Ａ', 2}, {'！', 2}, {'￥', 2},
		{'𠀀', 2}, {'豈', 2},
		{'⌛', 2}, {'☕', 2}, {'⚡', 2}, {'✅', 2}, {'❌', 2}, {'⭐', 2},
		{'🚀', 2}, {'😀', 2}, {'🌍', 2}, {'🤖', 2}, {'🪐', 2}, {'🟢', 2}, {'🆗', 2},
	} {
		if got := runeWidth(c.r); got != c.want {
			t.Errorf("runeWidth(%q U+%04X) = %d, want %d", c.r, c.r, got, c.want)
		}
	}
	for s, want := range map[string]int{
		"":                 0,
		"rota":             4,
		"仕事用アカウント":         16,
		"工作账户":             8,
		"한국어 계정":           11,
		"Ａｂｃ":              6,
		"ｶﾀｶﾅ":             4,
		"e\u0301te\u0301":  3,
		"a\u200bb":         2,
		"🚀 launch":         9,
		"⭐\ufe0f":          2,
		"ok (login ends…)": 16,
	} {
		if got := screenWidth(s); got != want {
			t.Errorf("screenWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

// unicode.Is looks a rune up by halving its table, which finds the wrong
// answer in a table out of order.
func TestWidthTablesAreInOrder(t *testing.T) {
	for name, tab := range map[string]*unicode.RangeTable{"wide": wide, "unseen": unseen} {
		last := rune(-1)
		for _, r := range tab.R16 {
			if rune(r.Lo) <= last || r.Hi < r.Lo || r.Stride != 1 {
				t.Errorf("%s: %04X–%04X is out of order", name, r.Lo, r.Hi)
			}
			last = rune(r.Hi)
		}
		for _, r := range tab.R32 {
			if rune(r.Lo) <= last || r.Hi < r.Lo || r.Stride != 1 {
				t.Errorf("%s: %05X–%05X is out of order", name, r.Lo, r.Hi)
			}
			last = rune(r.Hi)
		}
		if tab.LatinOffset != 0 {
			t.Errorf("%s: nothing in it is Latin-1", name)
		}
	}
}

// A cut or a wrap counts columns, never halves a wide character, and keeps a
// mark with its letter; ASCII is cut and wrapped exactly as it was.
func TestCutAndWrapCountColumns(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 4, "abc…"},
		{"abcdef", 6, "abcdef"},
		{"ab cdef", 4, "ab…"},
		{"仕事", 4, "仕事"},
		{"仕事用アカウント", 6, "仕事…"},
		{"仕事用アカウント", 7, "仕事用…"},
		{"ab仕事", 4, "ab…"},
		{"🚀🚀🚀", 4, "🚀…"},
		{"e\u0301e\u0301e\u0301", 2, "e\u0301…"},
	} {
		got := cut(c.in, c.n)
		if got != c.want {
			t.Errorf("cut(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if screenWidth(got) > c.n {
			t.Errorf("cut(%q, %d) is %d columns", c.in, c.n, screenWidth(got))
		}
	}
	// A cell is cut between its words where it has them, and where n falls
	// where it has none.
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"5h 100% (4h 12m)", 6, "5h…"},
		{"ok (ログインは2日後に切れます)", 10, "ok…"},
		{"仕事用アカウント", 7, "仕事用…"},
		{"仕事 用アカウント", 8, "仕事…"},
	} {
		if got := cutCell(c.in, c.n); got != c.want {
			t.Errorf("cutCell(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		n    int
		want []string
	}{
		{"仕事用アカウント", 5, []string{"仕事", "用ア", "カウ", "ント"}},
		{"仕事用 アカウント", 8, []string{"仕事用", "アカウン", "ト"}},
		{"工作账户@example.com", 10, []string{"工作账户", "@example.", "com"}},
		// A word's start goes on the line before when it fits there, in
		// columns.
		{"ab 工作账户@example.com", 12, []string{"ab 工作账户", "@example.com"}},
		{"a 仕事@x", 4, []string{"a", "仕事", "@x"}},
		{"🚀 launch 🚀 team", 9, []string{"🚀 launch", "🚀 team"}},
		{"🚀🚀🚀", 3, []string{"🚀", "🚀", "🚀"}},
		// Narrower than one character: one a line, and on to the next.
		{"仕事", 1, []string{"仕", "事"}},
	} {
		if got := wrapLine(c.in, c.n); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("wrapLine(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
	if got := clip("仕事用アカウントの名前がとても長い場合にはここで切れます", 44); screenWidth(got) > 44 || !strings.HasSuffix(got, "…") {
		t.Errorf("clip keeps a session's name to 44 columns: %q is %d", got, screenWidth(got))
	}
}

// wideGrid is a listing whose names take two columns a character: an
// account in Japanese, one in Chinese, and a label with emoji in it.
func wideGrid(width int) *grid {
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width, give: []int{7, 3}, keep: []int{6, 5, 2, 1, 0}}
	g.add("1", "3", "claude", "仕事用アカウント", "5h 3% (4h 12m)\n7d 29% (3d 2h)\nFable 48%", "95%", "just now", "ok")
	g.add("2", "5", "claude", "🚀 launch 🚀 team", "-", "95%", "2h ago", "re-auth needed ("+clearedReason+")")
	g.add("-", "7", "codex", "工作账户", "5h 0%", "90%", "3d ago", "ok (ログインは2日後に切れます)")
	return g
}

// cellsWhole checks that no character of a drawn table was halved: it is
// valid UTF-8, with nothing replaced.
func cellsWhole(t *testing.T, out string) {
	t.Helper()
	if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
		t.Fatalf("a character was halved:\n%s", out)
	}
}

// At 60 columns the Japanese account is as wide as the frame says, not
// four columns wider: every line is the frame's width, every border in the
// frame's columns, and the names read down their columns whole or cut with
// their start kept.
func TestTableFitsNamesInWideCharacters(t *testing.T) {
	out := drawn(wideGrid(60))
	framed(t, out, 60)
	cellsWhole(t, out)
	if w := screenWidth(strings.SplitN(out, "\n", 2)[0]); w != 60 {
		t.Fatalf("the frame is %d wide, and 60 was there to use:\n%s", w, out)
	}
	for id, name := range map[string]string{"3": "仕事用アカウント", "5": "🚀 launch 🚀 team", "7": "工作账户"} {
		got := squeeze(column(out, 3, id))
		if got != squeeze(name) && !(strings.HasSuffix(got, "…") && strings.HasPrefix(squeeze(name), strings.TrimSuffix(got, "…"))) {
			t.Fatalf("account %s reads %q down its column, of %q:\n%s", id, got, name, out)
		}
	}
	if got := squeeze(column(out, 7, "7")); got != squeeze("ok (ログインは2日後に切れます)") && !strings.HasSuffix(got, "…") {
		t.Fatalf("the Japanese status reads %q:\n%s", got, out)
	}
}

// From 30 columns to 200 the frame holds, and is never wider than the
// terminal unless nothing narrower can be drawn; at its natural width and
// over, the table is drawn as it is off a terminal.
func TestTableWithWideNamesFitsEveryWidth(t *testing.T) {
	floor := screenWidth(strings.SplitN(drawn(wideGrid(1)), "\n", 2)[0])
	natural := drawn(wideGrid(0))
	full := screenWidth(strings.SplitN(natural, "\n", 2)[0])
	for width := 30; width <= 200; width++ {
		out := drawn(wideGrid(width))
		framed(t, out, max(width, floor))
		cellsWhole(t, out)
		if width >= full && out != natural {
			t.Fatalf("%d columns is room enough:\n%s", width, out)
		}
	}
}

// Off a terminal a wide name pads its column by the columns it takes, so the
// border after it lines up with the frame's.
func TestTableOffATerminalLinesUpWideNames(t *testing.T) {
	out := drawn(wideGrid(0))
	framed(t, out, 1000)
	if !strings.Contains(out, "│ 仕事用アカウント  │") || !strings.Contains(out, "│ 工作账户"+strings.Repeat(" ", 10)+"│") {
		t.Fatalf("each name padded to its column:\n%s", out)
	}
}

// The listing itself, with accounts named in Japanese and Chinese, at a
// 60-column terminal and off one.
func TestListFitsNamesInWideCharacters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","name":"仕事用アカウント","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29}]}},
		{"id":2,"provider":"claude","email":"b@x","name":"工作账户 🚀","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "dead":true,"deadReason":%[2]q}]}`, time.Now().UnixMilli(), clearedReason))

	piped, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, piped)
	}
	table, _, _ := strings.Cut(piped, "┘\n")
	framed(t, table+"┘\n", 1000)

	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb, in: strings.NewReader(""), cols: 60}
	if err := c.list(nil); err != nil {
		t.Fatal(err)
	}
	table, notes, ok := strings.Cut(out.String(), "┘\n")
	if !ok {
		t.Fatalf("a table:\n%s", out.String())
	}
	framed(t, table+"┘\n", 60)
	cellsWhole(t, out.String())
	for _, line := range strings.Split(notes, "\n") {
		if screenWidth(line) > 60 {
			t.Fatalf("a note wider than the terminal: %q\n%s", line, notes)
		}
	}
}
