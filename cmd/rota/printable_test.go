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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/internal/share"
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
	// rota saw that login in the home a moment before Claude Code blanked it
	a.Extra = map[string]string{"home_synced_at": strconv.FormatInt(time.Now().UnixMilli(), 10), "home_synced_fp": a.Staged}
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

// An account with no --label is named by the email its provider gave, which
// may carry anything, so the line a run starts with names it with its
// control characters as spaces: the one before the CLI takes the terminal,
// and the one -v adds to a run with a prompt.
func TestARunNamesItsAccountWithItsControlCharactersAsSpaces(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":%s,"order":1,"token":{"accessToken":"t"}}]}`, jsonString("a\x1b]0;owned\x07@x")))
	answering(t, "claude")
	handover(t)

	_, errOut, code := call(t, "run", "1")
	if code != 0 || !strings.Contains(errOut, "rota: claude/a ]0;owned @x via ") {
		t.Fatalf("the CLI is handed the terminal under a clean name: %d %q", code, errOut)
	}
	if c := controls(errOut); c != "" {
		t.Fatalf("the line a run starts with has %s in it: %q", c, errOut)
	}
	out, errOut, code := call(t, "run", "-v", "1", "hello")
	if code != 0 || out != "ANSWERED\n" || !strings.Contains(errOut, "rota: claude/a ]0;owned @x\n") {
		t.Fatalf("-v names the account cleanly: %d %q %q", code, out, errOut)
	}
	if c := controls(errOut); c != "" {
		t.Fatalf("-v's line has %s in it: %q", c, errOut)
	}
}

// The watcher's log takes the store's warnings as stderr does, with their
// control characters as spaces: `tail -f` on it is a terminal too.
func TestTheWatchLogSaysAControlCharacterAsASpace(t *testing.T) {
	l := &watchLog{path: filepath.Join(t.TempDir(), "watch.log")}
	l.say("claude/a\x1b]0;owned\x07@x no longer keeps its login in its home")
	logged := readFile(t, l.path)
	if !strings.Contains(logged, "claude/a ]0;owned @x no longer keeps") {
		t.Fatalf("the warning, its control characters spaces: %q", logged)
	}
	if c := controls(logged); c != "" {
		t.Fatalf("the log has %s in it: %q", c, logged)
	}
}

// dirtyLabel is an address a provider might give, with an escape, a tab and
// a newline in it, and cleanLabel how a line of rota's own says it.
const dirtyLabel, cleanLabel = "x\x1b[2J\ty\nz@x", "x [2J y z@x"

// escLogin is an API-key provider whose login names its account with
// dirtyLabel, as a provider's profile may.
type escLogin struct{ cliFakeProvider }

func (escLogin) Name() string { return "t-esc-login" }
func (escLogin) Complete(_ context.Context, key string, _ map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: key, Identity: &rota.Identity{Email: dirtyLabel}}, nil
}

func init() { rota.Register(escLogin{}) }

// A label, a session's name or id, and a directory from the store are set
// in a line of rota's own with each control character in them, the newline
// too, a space: the line stays one line, ends as it always did, and nothing
// in it reaches the terminal as a command.
func TestALineOfRotasOwnSaysWhatItCarriesAsOneCleanLine(t *testing.T) {
	const dirtyDir, cleanDir = "/p\x1b]0;owned\x07\tq\nr", "/p ]0;owned  q r"
	store := func(t *testing.T, extra string) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("ROTA_HOME", home)
		writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
			{"id":1,"provider":"claude","email":%s,"order":1,"token":{"accessToken":"t"}%s}]}`, jsonString(dirtyLabel), extra))
	}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) (stdout, stderr string, code int)
		// want is what one line of stdout, or of stderr when it is
		// prefixed "2:", has in it.
		want []string
	}{
		{"a run's start", func(t *testing.T) (string, string, int) {
			store(t, "")
			answering(t, "claude")
			handover(t)
			return call(t, "run", "1")
		}, []string{"2:rota: claude/" + cleanLabel + " via claude"}},
		{"a run's session", func(t *testing.T) (string, string, int) {
			store(t, "")
			bin := t.TempDir()
			fakecli.Install(t, bin, "claude", fakecli.Lines(
				`{"type":"result","result":"ANSWERED","session_id":`+jsonString("s\x1b[2J\t1\n2")+`}`))
			t.Setenv("PATH", bin)
			return call(t, "run", "-v", "1", "hello")
		}, []string{"2:rota: claude/" + cleanLabel, "2:rota: session s [2J 1 2 — continue it with `rota run 1 <prompt> --resume s [2J 1 2`"}},
		{"a login", func(t *testing.T) (string, string, int) {
			t.Setenv("ROTA_HOME", t.TempDir())
			t.Setenv("PATH", t.TempDir())
			out, _, code := call(t, "login", "t-esc-login")
			m := regexp.MustCompile(`rota login ([0-9a-f]{6}) <key>`).FindStringSubmatch(out)
			if code != 0 || m == nil {
				t.Fatalf("begin: %d %q", code, out)
			}
			return call(t, "login", m[1], "sk-test-key")
		}, []string{"Added t-esc-login account 1 (" + cleanLabel + ")."}},
		{"rota show", func(t *testing.T) (string, string, int) {
			store(t, fmt.Sprintf(`,"cwd":%[1]s,"config_dir":%[1]s,"sessions":%[1]s`, jsonString(dirtyDir)))
			return call(t, "set", "1")
		}, []string{"#1 " + cleanLabel, "  runs in     " + cleanDir, "  configured  " + cleanDir,
			"  sessions    " + cleanDir + " (shared with any account pointed at it)"}},
		{"a move", func(t *testing.T) (string, string, int) {
			store(t, "")
			return call(t, "set", "1", "--order", "1")
		}, []string{"#1 " + cleanLabel + " is already 1st in the rotation.", "Rotation: #1 " + cleanLabel + "."}},
		{"rota remove", func(t *testing.T) (string, string, int) {
			store(t, "")
			return call(t, "remove", "1")
		}, []string{"Removed claude account 1 (" + cleanLabel + ")."}},
		{"the notes under rota list", func(t *testing.T) (string, string, int) {
			store(t, fmt.Sprintf(`,"quotaAt":%d,"quota":{"windows":[{"name":"5h","percent":3,"primary":true}],"note":"extra usage on"}`,
				time.Now().UnixMilli()))
			return call(t, "list")
		}, []string{"`rota run` without an id uses claude/" + cleanLabel + ", and moves on", "claude/" + cleanLabel + ": extra usage on"}},
		{"the credit note under rota list --short", func(t *testing.T) (string, string, int) {
			store(t, fmt.Sprintf(`,"credential":"apikey","noCreditUntil":%d`, time.Now().Add(time.Hour).UnixMilli()))
			return call(t, "list", "--short")
		}, []string{"claude/" + cleanLabel + ": a run on its API key was refused for want of credit"}},
		{"a name under --sessions", func(t *testing.T) (string, string, int) {
			store(t, "")
			claude := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", claude)
			transcript := filepath.Join(claude, "projects", "-p-q", "abcdef12-0000-0000-0000-000000000001.jsonl")
			if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
				t.Fatal(err)
			}
			body := `{"type":"user","cwd":` + jsonString(dirtyDir) + `,"message":{"role":"user","content":"hi"}}` + "\n" +
				`{"type":"ai-title","aiTitle":` + jsonString("n\x1b[2J\to\np") + `}` + "\n"
			if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			return call(t, "list", "--short", "--sessions")
		}, []string{"abcdef12  n [2J o  " + cleanDir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := tc.run(t)
			if code != 0 {
				t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
			}
			for name, text := range map[string]string{"stdout": out, "stderr": errOut} {
				if c := controls(text); c != "" {
					t.Fatalf("%s has %s in it: %q", name, c, text)
				}
			}
			for _, want := range tc.want {
				text := out
				if rest, ok := strings.CutPrefix(want, "2:"); ok {
					text, want = errOut, rest
				}
				if !slices.ContainsFunc(strings.Split(text, "\n"), func(l string) bool { return strings.Contains(l, want) }) {
					t.Fatalf("no line has %q in it:\n%s", want, text)
				}
			}
		})
	}
}

// The line a shared run opens with says the server's terminal id and the
// socket's path under the store with each control character in them, the
// newline too, as a space, in each of the three things it can be.
func TestTheOpeningLineSaysItsIDAndPathAsOneCleanLine(t *testing.T) {
	long := "/" + strings.Repeat("d/", 60) + "\x1b[2J\t\n.sock"
	for _, tc := range []struct{ id, path, want string }{
		{"t\x1b]0;owned\x07\t9\nx", "/s.sock", "as terminal t ]0;owned  9 x\n"},
		{"", "/s\x1b[2J\tq\nr.sock", "listening at /s [2J q r.sock; sharing"},
		{"", long, strings.Repeat("d/", 60) + " [2J  .sock is too long"},
	} {
		got := shareOpening("claude/a@x", "claude", tc.id, tc.path, share.ModeControl)
		if c := controls(got); c != "" || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "\n") || !strings.Contains(got, tc.want) {
			t.Fatalf("one clean line with %q in it: %q", tc.want, got)
		}
	}
}
