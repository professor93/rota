package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	rota "github.com/professor93/rota/lib"
)

// controls is the first control character but the newline in s, as a
// quoted string, or "" when there is none and s is UTF-8 through and through:
// what a terminal is sent from a table or a note must move its cursor only
// where the count of columns says.
func controls(s string) string {
	if !utf8.ValidString(s) {
		return "a byte that is not UTF-8"
	}
	for _, r := range s {
		if r != '\n' && unicode.IsControl(r) {
			return fmt.Sprintf("%q", r)
		}
	}
	return ""
}

// Every control character but the newline is a space: the C0 set with the
// tab and the escape in it, DEL, the C1 set, and every byte that is not
// UTF-8, one space a byte, a sequence cut short or an encoded surrogate
// included. Everything else is kept as it was: the newline that parts a
// cell's lines, wide characters, joined pictures, and a U+FFFD that was
// written as such.
func TestPrintableMakesEveryControlCharacterASpace(t *testing.T) {
	for r := rune(0); r <= 0x9f; r++ {
		if r == '\n' || r > 0x1f && r < 0x7f {
			continue
		}
		if got := printable("a" + string(r) + "b"); got != "a b" {
			t.Fatalf("U+%04X: %q", r, got)
		}
	}
	for b := 0x80; b <= 0xff; b++ {
		if got := printable("a" + string([]byte{byte(b)}) + "b"); got != "a b" {
			t.Fatalf("the byte %#x alone: %q", b, got)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"a\tb\r\x1b\x7f\xff\u0085\v\fc\nd\ufffd", "a b       c\nd\ufffd"},
		{"tab\there@x.com", "tab here@x.com"},
		{"\x1b[31mred\x1b[0m", " [31mred [0m"},
		{"\x1b]0;title\x07", " ]0;title "},
		{"line\r\nnext", "line \nnext"},
		{"a" + "中"[:2] + "b", "a  b"}, // cut short
		{"a\xed\xa0\x80b", "a   b"},   // U+D800, encoded
		{"a\xc0\xafb", "a  b"},        // "/", overlong
	} {
		if got := printable(tc.in); got != tc.want {
			t.Fatalf("printable(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, s := range []string{"", "a@x", "one\ntwo", "/Users/someone/项目 🚀\nx", "👨\u200d👩\u200d👧 ❤\ufe0f", "\ufffd"} {
		if got := printable(s); got != s {
			t.Fatalf("printable changed %q to %q", s, got)
		}
	}
}

// dirtyGrid is a listing whose text came from outside the program with
// control characters in it: an address with a tab, one coloured red with
// escapes, and a reason that would retitle the terminal. clean is the same
// listing with each of those characters a space. It names no column to wrap
// or to keep, so it is fitted the plainest way; how rota list fits its own
// table is the listing's test, below.
func dirtyGrid(width int, clean bool) *grid {
	tab, red, title := "tab\there@x.com", "red\x1b[31m@x.com\x1b[0m", "\x1b]0;owned\x07"
	if clean {
		tab, red, title = "tab here@x.com", "red [31m@x.com [0m", " ]0;owned "
	}
	g := &grid{header: []string{"#", "ID", "CLI", "ACCOUNT", "USAGE", "UNTIL", "CHECKED", "STATUS"}, width: width}
	g.add("1", "3", "claude", tab, "5h 3% (4h 12m)\n7d 29% (3d 2h)", "95%", "just now", "ok")
	g.add("2", "5", "claude", red, "-", "95%", "2h ago", "re-auth needed (the server said "+title+" no)")
	return g
}

// A control character in a cell is drawn as a space, at every width, so the
// table is the one its text would make with a space there instead: lined up,
// and with nothing in it that moves the cursor or recolours the terminal.
func TestTableDrawsAControlCharacterInACellAsASpace(t *testing.T) {
	for _, width := range []int{0, 40, 60, 90, 200} {
		got, want := drawn(dirtyGrid(width, false)), drawn(dirtyGrid(width, true))
		if c := controls(got); c != "" {
			t.Fatalf("at %d columns the table has %s in it:\n%q", width, c, got)
		}
		if got != want {
			t.Fatalf("at %d columns:\n%s\nwant:\n%s", width, got, want)
		}
		// Lined up, every border where the frame's is. How narrow it gets
		// is the fitting's to say, and its tests'.
		framed(t, got, 1000)
	}
}

// A note says a control character in what it carries as a space too, on a
// terminal and off one; its own lines are kept.
func TestSayDrawsAControlCharacterAsASpace(t *testing.T) {
	note := "\nclaude/red\x1b[31m@x.com: extra usage\t12.50 / 100.00 USD \x1b]0;owned\x07\n"
	var b bytes.Buffer
	say(&b, 0, note)
	if got, want := b.String(), "\nclaude/red [31m@x.com: extra usage 12.50 / 100.00 USD  ]0;owned \n"; got != want {
		t.Fatalf("off a terminal: %q, want %q", got, want)
	}
	b.Reset()
	say(&b, 30, note)
	got := b.String()
	if c := controls(got); c != "" {
		t.Fatalf("on a terminal the note has %s in it: %q", c, got)
	}
	if !strings.HasPrefix(got, "\n") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("the blank line before and the newline after stay: %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if screenWidth(line) > 30 {
			t.Fatalf("a line wider than the terminal: %q", line)
		}
	}
}

// A line under the sessions, a directory's or an error's, says a control
// character in it as a space, and a newline too: every line it prints is
// two spaces in, on a terminal and off one.
func TestSayInDrawsAControlCharacterAndANewlineAsASpace(t *testing.T) {
	line := "note: #1: open /Users/someone/a\tb\x1b[31m/new\nline/x: permission denied \x1b]0;owned\x07"
	var b bytes.Buffer
	sayIn(&b, 0, line)
	if got, want := b.String(), "  note: #1: open /Users/someone/a b [31m/new line/x: permission denied  ]0;owned \n"; got != want {
		t.Fatalf("off a terminal: %q, want %q", got, want)
	}
	b.Reset()
	sayIn(&b, 24, line)
	got := b.String()
	if c := controls(got); c != "" {
		t.Fatalf("on a terminal the line has %s in it: %q", c, got)
	}
	for _, l := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !strings.HasPrefix(l, "  ") || screenWidth(l) > 24 {
			t.Fatalf("every line two spaces in and within the terminal: %q in %q", l, got)
		}
	}
}

// jsonString is s as a JSON string, for a store a test writes.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// A store whose addresses, reason and provider note carry control
// characters lists them as spaces, in both listings, on a terminal and off
// one, under a frame that lines up. --json carries them as they are.
func TestListDrawsControlCharactersFromTheStoreAsSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":%[2]s,"order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true}],"note":%[3]s}},
		{"id":2,"provider":"claude","email":%[4]s,"order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "dead":true,"deadReason":%[5]s}]}`,
		time.Now().UnixMilli(), jsonString("tab\there@x.com"), jsonString("extra usage \x1b[2J12.50"),
		jsonString("red\x1b[31m@x.com\x1b[0m"), jsonString("invalid_grant: \x1b]0;owned\x07 reused")))

	check := func(name, out string, width int) {
		t.Helper()
		if c := controls(out); c != "" {
			t.Fatalf("%s has %s in it:\n%q", name, c, out)
		}
		table, _, ok := strings.Cut(out, "┘\n")
		if !ok {
			t.Fatalf("%s: a table:\n%s", name, out)
		}
		framed(t, table+"┘\n", width)
	}
	for _, args := range [][]string{{"list"}, {"list", "--short"}} {
		out, errOut, code := call(t, args...)
		if code != 0 {
			t.Fatalf("%v: %d %q %q", args, code, out, errOut)
		}
		check(strings.Join(args, " "), out, 1000)
		if c := controls(errOut); c != "" {
			t.Fatalf("%v: stderr has %s in it: %q", args, c, errOut)
		}
		if !strings.Contains(out, "│ tab here@x.com ") || !strings.Contains(out, "│ red [31m@x.com [0m ") {
			t.Fatalf("%v: both addresses, a space for each control character:\n%s", args, out)
		}
		if args[len(args)-1] == "list" {
			if !strings.Contains(out, "re-auth needed (invalid_grant:  ]0;owned  reused)") {
				t.Fatalf("the reason, its escapes spaces:\n%s", out)
			}
			if !strings.Contains(out, "claude/tab here@x.com: extra usage  [2J12.50") {
				t.Fatalf("the provider's note, its escape a space:\n%s", out)
			}
		}
	}
	for _, short := range []bool{false, true} {
		var out, errb bytes.Buffer
		c := &cli{out: &out, err: &errb, in: strings.NewReader(""), cols: 60}
		var args []string
		if short {
			args = []string{"--short"}
		}
		if err := c.list(args); err != nil {
			t.Fatal(err)
		}
		check(fmt.Sprintf("list %v at 60 columns", args), out.String(), 60)
		if c := controls(errb.String()); c != "" {
			t.Fatalf("list %v at 60 columns: stderr has %s in it: %q", args, c, errb.String())
		}
	}

	out, _, code := call(t, "--json", "list")
	if code != 0 {
		t.Fatalf("json: %d %q", code, out)
	}
	var doc struct {
		Accounts []struct {
			Email, Note, DeadReason string
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(doc.Accounts) != 2 || doc.Accounts[0].Email != "tab\there@x.com" || doc.Accounts[0].Note != "extra usage \x1b[2J12.50" ||
		doc.Accounts[1].Email != "red\x1b[31m@x.com\x1b[0m" || doc.Accounts[1].DeadReason != "invalid_grant: \x1b]0;owned\x07 reused" {
		t.Fatalf("json carries every value as it is: %+v", doc.Accounts)
	}
}

// escMeter is a metered provider whose usage endpoint fails with control
// characters in its error, as a provider's reply may.
type escMeter struct{ cliFakeProvider }

func (escMeter) Name() string { return "t-esc-meter" }
func (escMeter) Quota(context.Context, string) (*rota.Quota, error) {
	return nil, errors.New("the server said \x1b[2J\x1b]0;owned\x07 no")
}

func init() { rota.Register(escMeter{}) }

// A provider's error under a listing is said with its control characters
// as spaces as well.
func TestListWarnsWithAProvidersErrorAsSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"t-esc-meter","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	out, errOut, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	if c := controls(errOut); c != "" {
		t.Fatalf("the warning has %s in it: %q", c, errOut)
	}
	if !strings.Contains(errOut, "warning: t-esc-meter/a@x: quota: the server said  [2J ]0;owned  no\n") {
		t.Fatalf("the provider's error, its control characters spaces: %q", errOut)
	}
}

// A store's own warning names the account and may carry what a provider
// said, so it is said with its control characters as spaces: here the one
// a rescue of a home Claude Code blanked makes, under rota list --short.
func TestStoreWarningsSayAControlCharacterAsASpace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no login in a home")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"access_token": "A-new", "refresh_token": "R-new", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Token
	rota.ClaudeEndpoints.Token = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Token = old })

	dir := t.TempDir()
	t.Setenv("ROTA_HOME", dir)
	a := &rota.Account{ID: 1, Provider: "claude", Email: "a\x1b]0;owned\x07@x"}
	a.Token = rota.Token{Access: "A", Refresh: "R", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	a.StagedWritten()
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	writeStore(t, dir, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[%s]}`, raw))
	home := filepath.Join(dir, "homes", "claude-1")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	blanked := `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0}}`
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(blanked), 0o600); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := call(t, "list", "--short")
	if code != 0 || !strings.Contains(stderr, "warning: claude/a ]0;owned @x: ") || !strings.Contains(stderr, "refreshed it") {
		t.Fatalf("the rescue is said, the label's escape a space: %d %q", code, stderr)
	}
	if c := controls(stderr); c != "" {
		t.Fatalf("the warning has %s in it: %q", c, stderr)
	}
}

// The error a command ends with may carry a provider's reply or an
// account's label, so it is said with its control characters as spaces.
// --json carries it as it is.
func TestTheErrorACommandEndsWithSaysAControlCharacterAsASpace(t *testing.T) {
	cause := errors.New("claude/a\x1b]0;owned\x07@x: the server said \x1b[2J no")
	var out, errb bytes.Buffer
	c := &cli{out: &out, err: &errb, in: strings.NewReader("")}
	if code := c.report(cause); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if got, want := errb.String(), "error: claude/a ]0;owned @x: the server said  [2J no\n"; got != want || out.Len() != 0 {
		t.Fatalf("stderr %q, want %q; stdout %q", got, want, out.String())
	}

	out.Reset()
	errb.Reset()
	c.json = true
	c.report(cause)
	var doc struct{ Error string }
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.Error != cause.Error() || errb.Len() != 0 {
		t.Fatalf("json carries the error as it is: %v %q %q", err, out.String(), errb.String())
	}
}

// rota serve's error, in its log file as on stderr, says a path from its
// configuration with its control characters as spaces.
func TestServeSaysItsErrorWithAControlCharacterAsASpace(t *testing.T) {
	refusing(t) // a store, a token, and a server that does not wait for ever
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "no\x1b]0;owned\x07such", "cert.pem")
	path := filepath.Join(dir, "rota.log")
	out, errb, code := call(t, "serve", addr, "--refresh-every=0", "--log-file", path, "--tls-cert", cert, "--tls-key", cert)
	if code == 0 || out != "" || !strings.HasPrefix(errb, "error: ") || !strings.Contains(errb, "no ]0;owned such") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if c := controls(errb); c != "" {
		t.Fatalf("stderr has %s in it: %q", c, errb)
	}
	logged := readFile(t, path)
	if !strings.Contains(logged, "error: ") || !strings.Contains(logged, "no ]0;owned such") {
		t.Fatalf("the log says why it stopped:\n%s", logged)
	}
	if c := controls(logged); c != "" {
		t.Fatalf("the log has %s in it: %q", c, logged)
	}
}
