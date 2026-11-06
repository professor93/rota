package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
func sessionsAt(width int) string {
	var out bytes.Buffer
	c := &cli{out: &out, err: &out, cols: width}
	c.showSessions(longReport(), width)
	return out.String()
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

func starts(row string) []int {
	var at []int
	for _, m := range cell.FindAllStringIndex(row, -1) {
		at = append(at, utf8.RuneCountInString(row[:m[0]]))
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
				if n := utf8.RuneCountInString(line); n > width {
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
			if utf8.RuneCountInString(who) > cutSoft && !strings.HasSuffix(note, "session af7fda4d") {
				t.Fatalf("the note cut to %q while the account is %q:\n%s", note, who, out)
			}
			if dir := rowCells(inst[0])[3]; utf8.RuneCountInString(who) > cutSoft && utf8.RuneCountInString(dir) < cutSoft {
				t.Fatalf("the directory cut to %q while the account is %q:\n%s", dir, who, out)
			}
			if who := rowCells(sess[1])[0]; utf8.RuneCountInString(who) > cutSoft {
				if name, least := row[3], cutCell(full.Sessions[2].Name, cutSoft); utf8.RuneCountInString(name) < utf8.RuneCountInString(least) {
					t.Fatalf("the title cut to %q while the account is %q:\n%s", name, who, out)
				}
				if dir := row[4]; utf8.RuneCountInString(dir) < cutSoft {
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
		if n := utf8.RuneCountInString(line); n > 60 {
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
	} {
		if got := cutStart(c.in, c.n); got != c.want {
			t.Errorf("cutStart(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
