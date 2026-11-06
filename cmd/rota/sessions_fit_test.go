package main

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/professor93/rota/sessions"
)

// longReport is a sessions listing with what makes one too wide for half a
// screen: a long address, long titles, and paths deep in a home directory.
func longReport() sessions.Report {
	return sessions.Report{
		Instances: []sessions.Instance{
			{Kind: "cli", Provider: "claude", Account: 1, Label: "manager.ican@gmail.com",
				Dir: "/Users/someone/GolandProjects/a-rather-long-project-name/services/api", PID: 4242,
				Session: "af7fda4d-0000-0000-0000-000000000001", Since: time.Now().Add(-90 * time.Minute)},
			{Kind: "GoLand", Dir: "/Users/someone/GolandProjects/a-rather-long-project-name", PID: 4243},
		},
		Sessions: []sessions.Session{
			{Provider: "claude", ID: "497f1383-0000-0000-0000-000000000003", Shared: true,
				Name: "rota run account resolution across every provider it knows about", Dir: "/Users/someone/src/x"},
			{Account: 2, Label: "a.very.long.address.for.work@example.com", Provider: "claude", ID: "01a048be-0000-0000-0000-000000000002",
				Name: "short", Dir: "/Users/someone/work/clients/northwind/infrastructure/terraform"},
			{Account: 1, Label: "manager.ican@gmail.com", Provider: "codex", ID: "abcdef12-0000-0000-0000-000000000001",
				Name: "Review of the account rotation when a refresh token is reused", Dir: "/Users/someone/GolandProjects/a-rather-long-project-name/services/api"},
			{Account: 1, Label: "manager.ican@gmail.com", Provider: "codex", ID: "0badcafe"},
		},
		Shared: &sessions.Shared{Dir: "/Users/someone/.claude", Sessions: 2648, Projects: 146},
		Notes:  []string{`could not read the process list: exec: "ps": executable file not found in $PATH`},
	}
}

// sessionsAt is the listing of longReport on a terminal width columns wide,
// 0 being no terminal at all.
func sessionsAt(width int) string { return sessionsOf(longReport(), width) }

// sessionsOf is the listing of rep on a terminal width columns wide.
func sessionsOf(rep sessions.Report, width int) string {
	var out bytes.Buffer
	c := &cli{out: &out, err: &out, cols: width}
	c.showSessions(rep, width)
	return out.String()
}

// wideReport is longReport with a title in Japanese and directories in
// Chinese, which take two columns a character.
func wideReport() sessions.Report {
	rep := longReport()
	rep.Instances[0].Dir = "/Users/someone/项目/客户端/服务/接口"
	rep.Sessions[2].Name = "アカウントの切り替えでリフレッシュトークンが再利用されたときの確認"
	rep.Sessions[2].Dir = "/Users/someone/项目/客户端/服务/接口/测试"
	return rep
}

// Off a terminal the sessions are lined up exactly as they were before a
// listing could fit one, and so they are on a terminal wide enough.
func TestSessionsOffATerminalAreListedAsBefore(t *testing.T) {
	want := golden(t, "sessions-plain.golden")
	if got := sessionsAt(0); got != want {
		t.Fatalf("the listing changed:\n%s\nwant:\n%s", got, want)
	}
	if got := sessionsAt(200); got != want {
		t.Fatalf("200 columns is room enough:\n%s\nwant:\n%s", got, want)
	}
}

// sections is the rows of each table of a sessions listing of longReport,
// a line each: what is under its heading and above what follows it.
func sections(out string) (instances, sessions []string) {
	_, rest, _ := strings.Cut(out, "Running instances:\n")
	inst, rest, _ := strings.Cut(rest, "\nSessions:\n")
	sess, _, _ := strings.Cut(rest, "  /Users/someone/.claude")
	return rows(inst), rows(sess)
}

func rows(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// cell is a run of words one space apart; two spaces or more part cells.
var cell = regexp.MustCompile(`\S+( \S+)*`)

// rowCells is a row of a plain table, cell by cell, and starts where each cell
// starts on it.
func rowCells(row string) []string { return cell.FindAllString(row, -1) }

// starts is the screen column each cell of a row starts in.
func starts(row string) []int {
	var at []int
	for _, m := range cell.FindAllStringIndex(row, -1) {
		at = append(at, screenWidth(row[:m[0]]))
	}
	return at
}

// On a narrow terminal every line fits, every row stays one line with its
// columns lined up, and what is cut is the free text: the session id is
// whole at every width, and the account until the terminal is too narrow
// for even that.
func TestSessionsFitANarrowTerminal(t *testing.T) {
	for _, width := range []int{120, 100, 80, 60, 50, 40} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := sessionsAt(width)
			for _, line := range strings.Split(out, "\n") {
				if n := screenWidth(line); n > width {
					t.Fatalf("a line %d wide at %d columns: %q\n%s", n, width, line, out)
				}
			}
			inst, sess := sections(out)
			if len(inst) != 2 || len(sess) != 4 {
				t.Fatalf("one line a row: %d instances, %d sessions\n%s", len(inst), len(sess), out)
			}
			for _, rows := range [][]string{inst, sess} {
				want := fmt.Sprint(starts(rows[0]))
				for _, r := range rows {
					if got := fmt.Sprint(starts(r)); got != want {
						t.Fatalf("columns at %s, the first row's at %s:\n%s", got, want, out)
					}
				}
			}
			for _, id := range []string{"497f1383", "01a048be", "abcdef12", "0badcafe"} {
				if !strings.Contains(out, "  "+id+"  ") {
					t.Fatalf("the id %s, whole:\n%s", id, out)
				}
			}
			if width >= 80 && strings.Count(out, "#1 manager.ican@gmail.com  ") != 3 {
				t.Fatalf("the account, whole in every row it is in:\n%s", out)
			}
			// An account is cut where the width falls, never back to the
			// space after its number, which would leave no account at all.
			if m := regexp.MustCompile(`#\d+…`).FindString(out); m != "" {
				t.Fatalf("an account cut to %q:\n%s", m, out)
			}
			// What is cut keeps the part that says the most: a title its
			// start, a path its end, and the note the session id it ends in.
			full := longReport()
			row := rowCells(sess[2])
			if name := row[3]; !strings.HasSuffix(name, "…") || !strings.HasPrefix(full.Sessions[2].Name, strings.TrimSuffix(name, "…")) {
				t.Fatalf("the title keeps its start: %q\n%s", name, out)
			}
			if dir := row[4]; !strings.HasPrefix(dir, "…") || !strings.HasSuffix(full.Sessions[2].Dir, strings.TrimPrefix(dir, "…")) {
				t.Fatalf("the path keeps its end: %q\n%s", dir, out)
			}
			if !strings.HasSuffix(inst[0], "  …af7fda4d") && !strings.HasSuffix(inst[0], "session af7fda4d") {
				t.Fatalf("the note keeps the session id at its end, whole: %q\n%s", inst[0], out)
			}
			// The free text keeps cutSoft columns until the account has
			// given way as far, and the note "…session af7fda4d"; the id
			// goes no sooner than the account.
			who, note := rowCells(inst[0])[2], rowCells(inst[0])[4]
			if screenWidth(who) > cutSoft && !strings.HasSuffix(note, "session af7fda4d") {
				t.Fatalf("the note cut to %q while the account is %q:\n%s", note, who, out)
			}
			if dir := rowCells(inst[0])[3]; screenWidth(who) > cutSoft && screenWidth(dir) < cutSoft {
				t.Fatalf("the directory cut to %q while the account is %q:\n%s", dir, who, out)
			}
			if who := rowCells(sess[1])[0]; screenWidth(who) > cutSoft {
				if name, least := row[3], cutCell(full.Sessions[2].Name, cutSoft); screenWidth(name) < screenWidth(least) {
					t.Fatalf("the title cut to %q while the account is %q:\n%s", name, who, out)
				}
				if dir := row[4]; screenWidth(dir) < cutSoft {
					t.Fatalf("the path cut to %q while the account is %q:\n%s", dir, who, out)
				}
			}
			// The lines under the tables wrap two spaces in, and say what
			// they said.
			tail := out[strings.Index(out, "  /Users/someone/.claude"):]
			for _, line := range strings.Split(strings.TrimSuffix(tail, "\n"), "\n") {
				if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
					t.Fatalf("two spaces in: %q\n%s", line, out)
				}
			}
			want := sessionsAt(0)
			want = want[strings.Index(want, "  /Users/someone/.claude"):]
			if squeeze(strings.Join(strings.Fields(tail), " ")) != squeeze(strings.Join(strings.Fields(want), " ")) {
				t.Fatalf("the notes say what they said:\n%s\nwant:\n%s", tail, want)
			}
		})
	}
	// 120 columns cuts only the free text.
	if out := sessionsAt(120); !strings.Contains(out, "#2 a.very.long.address.for.work@example.com  ") {
		t.Fatalf("at 120 the longest account is whole:\n%s", out)
	}
}

// No terminal is too narrow to list on, an empty section included: each
// row is still one line, and an empty section says so.
func TestSessionsAtTheNarrowestWidths(t *testing.T) {
	full := longReport()
	for _, rep := range []sessions.Report{{}, {Instances: full.Instances}, {Sessions: full.Sessions},
		{Instances: full.Instances, Sessions: full.Sessions}} {
		for width := range 4 {
			var out bytes.Buffer
			c := &cli{out: &out, err: &out, cols: width}
			c.showSessions(rep, width)
			inst, sess := sections(out.String())
			if want := max(len(rep.Instances), 1); len(inst) != want {
				t.Fatalf("%d instances at %d columns, %d lines:\n%s", len(rep.Instances), width, len(inst), out.String())
			}
			if want := max(len(rep.Sessions), 1); len(sess) != want {
				t.Fatalf("%d sessions at %d columns, %d lines:\n%s", len(rep.Sessions), width, len(sess), out.String())
			}
			if len(rep.Instances) == 0 && inst[0] != "  none" || len(rep.Sessions) == 0 && sess[0] != "  none" {
				t.Fatalf("an empty section says so at %d columns:\n%s", width, out.String())
			}
		}
	}
}

// The listing passes its terminal on: `rota list --sessions` at 60 columns
// fits its sessions as well as its accounts.
func TestListSessionsFitTheTerminalItIsOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	cwd := "/Users/someone/GolandProjects/a-rather-long-project-name/services/api"
	transcript := filepath.Join(claude, "projects", strings.ReplaceAll(cwd, "/", "-"), "abcdef12-0000-0000-0000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","cwd":"`+cwd+`","message":{"role":"user","content":"look"}}`+"\n"+
		`{"type":"ai-title","aiTitle":"Review of the account rotation when a refresh token is reused"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	piped, _, code := call(t, "list", "--short", "--sessions")
	if code != 0 {
		t.Fatalf("%d %q", code, piped)
	}
	if !strings.Contains(piped, cwd) {
		t.Fatalf("off a terminal the path is whole:\n%s", piped)
	}
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb, in: strings.NewReader(""), cols: 60}
	if err := c.list([]string{"--short", "--sessions"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if n := screenWidth(line); n > 60 {
			t.Fatalf("a line %d wide at 60 columns: %q\n%s", n, line, out.String())
		}
	}
	if !strings.Contains(out.String(), "  abcdef12  ") {
		t.Fatalf("the id, whole:\n%s", out.String())
	}
}

func TestCutStart(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"/Users/someone/src/api", 30, "/Users/someone/src/api"},
		{"/Users/someone/src/api", 9, "…/src/api"},
		{"/Users/someone/src/api", 2, "…i"},
		// Not in part of a word, when there is a space to go forward to.
		{"1h ago, session af7fda4d", 20, "…session af7fda4d"},
		{"1h ago, session af7fda4d", 17, "…session af7fda4d"},
		{"1h ago, session af7fda4d", 9, "…af7fda4d"},
		{"1h ago, session af7fda4d", 5, "…da4d"},
		// Counted in screen columns: a wide character the cut would halve
		// goes, and so does a mark whose letter went.
		{"/项目/客户端/服务", 7, "…/服务"},
		{"/项目/客户端/服务", 6, "…/服务"},
		{"/项目/客户端/服务", 5, "…服务"},
		{"/项目/客户端/服务", 4, "…务"},
		{"ae\u0301x", 2, "…x"},
		// An emoji of several runes, and a symbol asked to be a picture, is
		// two columns kept whole or not at all, as screenWidth counts it.
		{"/x/☀\ufe0f☀\ufe0f☀\ufe0f", 4, "…☀\ufe0f"},
		{"/x/☀\ufe0f☀\ufe0f☀\ufe0f", 5, "…☀\ufe0f☀\ufe0f"},
		{"/x/🇯🇵🇺🇸", 4, "…🇺🇸"},
		{"/x/🇯🇵🇺🇸", 3, "…🇺🇸"},
		{"/x/🇯🇵🇺🇸", 2, "…"},
		{"/x/👍🏽👍🏽", 4, "…👍🏽"},
		{"/x/👨\u200d👩\u200d👧👨\u200d👩\u200d👧", 4, "…👨\u200d👩\u200d👧"},
		{"/x/👨\u200d👩\u200d👧👨\u200d👩\u200d👧", 5, "…👨\u200d👩\u200d👧👨\u200d👩\u200d👧"},
		// Going forward to a space needs two characters after it, counted
		// as characters: a letter with its mark, or an emoji, is one.
		{"abcdef x", 5, "…ef x"},
		{"abcdef e\u0301", 5, "…ef e\u0301"},
		{"abcdef 👍🏽", 6, "…ef 👍🏽"},
		{"abcdef 仕事", 7, "…仕事"},
	} {
		if got := cutStart(c.in, c.n); got != c.want {
			t.Errorf("cutStart(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

// At every width a cut from the start is no wider than it was asked to be,
// and keeps whole characters of the end: what follows its "…" ends the line
// and starts where a character of it starts.
func TestCutStartKeepsWholeCharactersAtEveryWidth(t *testing.T) {
	for _, in := range []string{
		"/Users/someone/src/api",
		"1h ago, session af7fda4d",
		"/Users/someone/项目/客户端/服务/接口/测试",
		"/x/☀\ufe0f☀\ufe0f☀\ufe0f",
		"/a/🇯🇵🇺🇸/👍🏽👍🏽/👨\u200d👩\u200d👧/1\ufe0f\u20e3",
		"ae\u0301e\u0301 👩\u200d💻 x 仕事 y",
		"アカウントの切り替え 🚀 launch 🇯🇵 team",
	} {
		r := []rune(in)
		at := map[int]bool{} // the byte where each character starts
		for i := 0; i < len(r); {
			at[len(string(r[:i]))] = true
			i, _ = char(r, i)
		}
		for n := 1; n <= screenWidth(in)+1; n++ {
			got := cutStart(in, n)
			if screenWidth(got) > n {
				t.Errorf("cutStart(%q, %d) = %q, %d columns", in, n, got, screenWidth(got))
			}
			if screenWidth(in) <= n {
				if got != in {
					t.Errorf("cutStart(%q, %d) = %q, and it fit", in, n, got)
				}
				continue
			}
			kept, ok := strings.CutPrefix(got, "…")
			if !ok || !strings.HasSuffix(in, kept) || !at[len(in)-len(kept)] && kept != "" {
				t.Errorf("cutStart(%q, %d) = %q, not whole characters of its end", in, n, got)
			}
		}
	}
}

// A title or a directory in Chinese or Japanese is measured by the columns
// it takes on screen: off a terminal its column is padded by them so what
// follows lines up, and on a narrow one every line fits, every row's cells
// start where the first row's do, and no character is halved.
func TestSessionsLineUpWideCharacters(t *testing.T) {
	for _, width := range []int{0, 200, 120, 80, 60, 40} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			out := sessionsOf(wideReport(), width)
			cellsWhole(t, out)
			for _, line := range strings.Split(out, "\n") {
				if n := screenWidth(line); width > 0 && n > width {
					t.Fatalf("a line %d wide at %d columns: %q\n%s", n, width, line, out)
				}
			}
			inst, sess := sections(out)
			if len(inst) != 2 || len(sess) != 4 {
				t.Fatalf("one line a row: %d instances, %d sessions\n%s", len(inst), len(sess), out)
			}
			for _, rows := range [][]string{inst, sess} {
				want := fmt.Sprint(starts(rows[0]))
				for _, r := range rows {
					if got := fmt.Sprint(starts(r)); got != want {
						t.Fatalf("columns at %s, the first row's at %s:\n%s", got, want, out)
					}
				}
			}
			for _, id := range []string{"497f1383", "01a048be", "abcdef12", "0badcafe"} {
				if !strings.Contains(out, "  "+id+"  ") {
					t.Fatalf("the id %s, whole:\n%s", id, out)
				}
			}
		})
	}
}

// A plain table of one-column characters with no control character in it
// is what text/tabwriter made of it, byte for byte, empty cells and an
// empty last cell included: the table above, and a few hundred of printable
// ASCII drawn at random, each row as many cells as the next.
func TestPlainIsTabwriterForNarrowCharacters(t *testing.T) {
	tables := [][][]string{{
		{"●", "claude", "#1 a@x", "/a/b", "1h ago, session af7fda4d"},
		{"●", "GoLand", "-", "", ""},
		{"", "", "", "", "pid 1"},
		{"x", "a longer cell than the rest", "", "/dir with spaces", ""},
	}}
	rnd := rand.New(rand.NewPCG(1, 2))
	for range 300 {
		cols, n := 1+rnd.IntN(6), 1+rnd.IntN(8)
		var rows [][]string
		for range n {
			r := make([]string, cols)
			for i := range r {
				b := make([]byte, rnd.IntN(13))
				for k := range b {
					b[k] = byte(' ' + rnd.IntN('~'-' '+1))
				}
				r[i] = string(b)
			}
			rows = append(rows, r)
		}
		tables = append(tables, rows)
	}
	for _, rows := range tables {
		var want bytes.Buffer
		tw := tabwriter.NewWriter(&want, 0, 0, 2, ' ', 0)
		g := grid{plain: true}
		for _, r := range rows {
			fmt.Fprintf(tw, "  %s\n", strings.Join(r, "\t"))
			g.add(r...)
		}
		_ = tw.Flush()
		var got bytes.Buffer
		g.render(&got)
		if got.String() != want.String() {
			t.Fatalf("%q\ngot:\n%q\nwant:\n%q", rows, got.String(), want.String())
		}
	}
}

// A control character in a cell — a tab, a carriage return, an escape, a
// DEL, a byte that is not UTF-8 — is drawn as a space, so a directory with
// a tab in it lines up like any other; the newline still parts a cell's
// lines, and U+FFFD that was written as such is kept.
func TestPlainDrawsAControlCharacterAsASpace(t *testing.T) {
	if got, want := printable("a\tb\r\x1b\x7f\xff\u0085\v\fc\nd\ufffd"), "a b       c\nd\ufffd"; got != want {
		t.Fatalf("printable = %q, want %q", got, want)
	}
	if s := "/Users/someone/项目 🚀\nx"; printable(s) != s {
		t.Fatalf("printable changed %q", s)
	}
	dirty := [][]string{
		{"●", "claude", "#1 a@x", "/a\tb/c\x1b[31md", "1h ago, session af7fda4d"},
		{"●", "GoLand", "-", "/x\ry\x7fz\xffw", "pid 1"},
		{"●", "codex", "#2 b@x", "/plain/dir", "pid 2"},
	}
	for _, width := range []int{0, 60, 40, 30} {
		var g, clean grid
		g = grid{plain: true, width: width, cutBy: map[int]func(string, int) string{3: cutStart}}
		clean = g
		for _, r := range dirty {
			g.add(r...)
			c := make([]string, len(r))
			for i := range r {
				c[i] = strings.Map(func(r rune) rune {
					if r == utf8.RuneError || unicode.IsControl(r) {
						return ' '
					}
					return r
				}, r[i])
			}
			clean.add(c...)
		}
		var got, want bytes.Buffer
		g.render(&got)
		clean.render(&want)
		if got.String() != want.String() {
			t.Fatalf("at %d:\n%s\nwant:\n%s", width, got.String(), want.String())
		}
		if strings.ContainsFunc(got.String(), func(r rune) bool { return r != '\n' && unicode.IsControl(r) }) || !utf8.ValidString(got.String()) {
			t.Fatalf("a control character drawn at %d: %q", width, got.String())
		}
		lines := rows(got.String())
		for _, l := range lines {
			if fmt.Sprint(starts(l)) != fmt.Sprint(starts(lines[0])) {
				t.Fatalf("out of line at %d:\n%s", width, got.String())
			}
		}
	}
}

// A newline in a directory or a name is a space too, so its row stays one
// line, lined up with the rest, on a terminal of any width and off one.
func TestPlainDrawsANewlineAsASpace(t *testing.T) {
	broken, spaced := longReport(), longReport()
	broken.Instances[0].Dir = "/Users/someone/a\nb/services/api"
	spaced.Instances[0].Dir = "/Users/someone/a b/services/api"
	broken.Sessions[1].Dir = "/Users/someone/work\n\nterraform"
	spaced.Sessions[1].Dir = "/Users/someone/work  terraform"
	broken.Sessions[0].Name = "two\nlines"
	spaced.Sessions[0].Name = "two lines"
	for _, width := range []int{0, 200, 80, 60, 40, 24} {
		got, want := sessionsOf(broken, width), sessionsOf(spaced, width)
		if got != want {
			t.Fatalf("at %d:\n%s\nwant:\n%s", width, got, want)
		}
	}
	g := grid{plain: true}
	g.add("●", "claude", "/a\nb", "pid 1")
	g.add("●", "GoLand", "/c", "pid 2")
	var out bytes.Buffer
	g.render(&out)
	if want := "  ●  claude  /a b  pid 1\n  ●  GoLand  /c    pid 2\n"; out.String() != want {
		t.Fatalf("got %q, want %q", out.String(), want)
	}
}

// emojiReport is longReport with emoji in its titles and directories: a
// flag, a skin tone, a joined pair, and a symbol asked to be a picture.
func emojiReport() sessions.Report {
	rep := longReport()
	rep.Instances[0].Dir = "/Users/someone/プロジェクト/🚀/☀\ufe0f☀\ufe0f/🇯🇵/api"
	rep.Sessions[0].Name = "🇯🇵 launch 👍🏽 review 👩\u200d💻 ☀\ufe0f weather"
	rep.Sessions[1].Label = "仕事用アカウント@example.com"
	rep.Sessions[2].Dir = "/Users/someone/项目/👩\u200d💻/🇯🇵🇯🇵/☀\ufe0f☀\ufe0f☀\ufe0f/测试"
	return rep
}

// From no terminal to 220 columns, with names in Chinese and Japanese, with
// emoji, and with nothing to list, nothing panics, every row is one line,
// no character is halved, and from the narrowest width the tables and their
// headings can be drawn at, no line is wider than the terminal and the
// cells line up. (The lines under the tables wrap as sayIn does, which is
// tested on its own; here they would only blur where a table ends.)
func TestSessionsAtEveryWidth(t *testing.T) {
	wide, emoji := wideReport(), emojiReport()
	wide.Shared, wide.Notes, emoji.Shared, emoji.Notes = nil, nil, nil, nil
	for name, rep := range map[string]sessions.Report{"wide": wide, "emoji": emoji, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			// floor is the widest row of the tables at their narrowest, or
			// the widest heading.
			floor := screenWidth("Running instances:")
			inst, sess := sections(sessionsOf(rep, 1))
			for _, l := range append(inst, sess...) {
				floor = max(floor, screenWidth(l))
			}
			if floor > 24 {
				t.Fatalf("the tables are %d columns at their narrowest:\n%s", floor, sessionsOf(rep, 1))
			}
			for width := 0; width <= 220; width++ {
				out := sessionsOf(rep, width)
				cellsWhole(t, out)
				if strings.Count(out, "🏽") != strings.Count(out, "👍🏽") || strings.Count(out, "\ufe0f") != strings.Count(out, "☀\ufe0f") {
					t.Fatalf("an emoji was split at %d:\n%s", width, out)
				}
				inst, sess := sections(out)
				if len(inst) != max(len(rep.Instances), 1) || len(sess) != max(len(rep.Sessions), 1) {
					t.Fatalf("one line a row at %d:\n%s", width, out)
				}
				if width < floor {
					continue
				}
				for _, line := range strings.Split(out, "\n") {
					if n := screenWidth(line); width > 0 && n > width {
						t.Fatalf("a line %d wide at %d columns: %q\n%s", n, width, line, out)
					}
				}
				for _, rows := range [][]string{inst, sess} {
					want := fmt.Sprint(starts(rows[0]))
					for _, r := range rows {
						if got := fmt.Sprint(starts(r)); got != want {
							t.Fatalf("columns at %s, the first row's at %s, at %d:\n%s", got, want, width, out)
						}
					}
				}
			}
		})
	}
}
