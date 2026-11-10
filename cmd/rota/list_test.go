package main

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	rota "github.com/professor93/rota/lib"
)

// cells is a bordered line without its frame: the words in it, in order.
func cells(line string) []string {
	return strings.Fields(strings.ReplaceAll(line, "│", " "))
}

// The listing is a framed table with a space of padding on each side of a
// cell and nothing more: the two widest headers are a mark and a short
// word, and each usage window sits on a line of its own under USAGE. Rows
// that span lines are ruled apart, so a row reads down rather than across.
func TestListIsAFramedTableWithOneUsageWindowPerLine(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	// Fresh readings, so the listing asks no provider anything.
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "quota":{"windows":[{"name":"5h","percent":3,"primary":true},{"name":"7d","percent":29},{"name":"Fable","percent":48,"scoped":true}]}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d}]}`,
		time.Now().UnixMilli()))

	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 9 {
		t.Fatalf("a frame, a header, a ruled row of three lines, a row, a frame:\n%s", out)
	}
	top, header, rule, row, second, third, between, next, bottom := lines[0], lines[1], lines[2], lines[3], lines[4], lines[5], lines[6], lines[7], lines[8]
	if !strings.HasPrefix(top, "┌") || !strings.HasSuffix(top, "┐") || !strings.HasPrefix(bottom, "└") || !strings.HasSuffix(bottom, "┘") {
		t.Fatalf("framed top and bottom:\n%s", out)
	}
	if got := strings.Join(cells(header), " "); got != "# ID CLI ACCOUNT USAGE UNTIL CHECKED STATUS" {
		t.Fatalf("header: %q", header)
	}
	if !strings.HasPrefix(rule, "├") || !strings.Contains(rule, "┼") || !strings.HasSuffix(rule, "┤") {
		t.Fatalf("a rule under the header: %q", rule)
	}
	if !strings.Contains(row, "│ a@x") || !strings.Contains(row, "│ 5h 3%") || !strings.Contains(row, "│ ok") || strings.Contains(row, "7d") {
		t.Fatalf("the row carries the account and its first window only: %q", row)
	}
	if got := strings.Join(cells(second), " ") + "|" + strings.Join(cells(third), " "); got != "7d 29%|Fable 48%" {
		t.Fatalf("the other windows follow, one per line, nothing else on them: %q %q", second, third)
	}
	col := strings.Index(header, "USAGE")
	if strings.Index(row, "5h") != col || strings.Index(second, "7d") != col || strings.Index(third, "Fable") != col {
		t.Fatalf("every window starts under USAGE (byte %d):\n%s", col, out)
	}
	if !strings.HasPrefix(between, "├") || !strings.Contains(next, "│ b@x") {
		t.Fatalf("rows that span lines are ruled apart: %q %q", between, next)
	}
	// Minimal padding: one space each side of the widest cell, so the
	// header's ACCOUNT column is exactly as wide as the longest address.
	if !strings.Contains(header, "│ ACCOUNT │") || !strings.Contains(row, "│ a@x     │") {
		t.Fatalf("one space of padding around the widest cell in a column:\n%s", out)
	}
	width := utf8.RuneCountInString(top)
	for i, line := range lines[:9] {
		if utf8.RuneCountInString(line) != width {
			t.Fatalf("line %d is not the table's width %d: %q", i+1, width, line)
		}
	}
}

// The short listing wears the same frame and headers. Its rows are one line
// each, so nothing rules them apart.
func TestShortListWearsTheSameFrame(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)
	out, _, code := call(t, "list", "--short")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 6 || !strings.HasPrefix(lines[0], "┌") || !strings.HasPrefix(lines[2], "├") || !strings.HasPrefix(lines[5], "└") {
		t.Fatalf("frame, header, rule, two rows, frame — six lines:\n%s", out)
	}
	if got := strings.Join(cells(lines[1]), " "); got != "# ID CLI ACCOUNT USAGE CHECKED" {
		t.Fatalf("header: %q", lines[1])
	}
	if strings.HasPrefix(lines[4], "├") || !strings.Contains(lines[3], "a@x") || !strings.Contains(lines[4], "b@x") {
		t.Fatalf("one-line rows are not ruled apart:\n%s", out)
	}
}

// A dead account says why in the STATUS cell: "re-auth needed" on its own
// leaves a reused refresh token and a revoked credential looking alike.
func TestListShowsWhyADeadAccountNeedsReauth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%d,
		 "dead":true,"deadReason":"invalid_grant: refresh token reused"}]}`, time.Now().UnixMilli()))

	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, "re-auth needed (invalid_grant: refresh token reused)") {
		t.Fatalf("the status cell must carry the refusal:\n%s", out)
	}

	out, _, code = call(t, "--json", "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, `"deadReason"`) || !strings.Contains(out, "invalid_grant: refresh token reused") {
		t.Fatalf("json carries the reason as its own field:\n%s", out)
	}
}

// A login about to end says so in the STATUS cell, whatever state the account
// is otherwise in; nothing changes shape, and a login far from its end adds
// nothing.
func TestListSaysInTheStatusCellWhenALoginEnds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	now := time.Now()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":4,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "extra":{"refresh_token_expires_at":"%[2]d"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "extra":{"refresh_token_expires_at":"%[3]d"}},
		{"id":3,"provider":"claude","email":"c@x","order":3,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "extra":{"refresh_token_expires_at":"%[4]d"}}]}`,
		now.UnixMilli(), now.Add(50*time.Hour).UnixMilli(), now.Add(-2*time.Hour-time.Minute).UnixMilli(), now.Add(30*24*time.Hour).UnixMilli()))
	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, "ok (login ends in 2d)") || !strings.Contains(out, "ok (login ended 2h ago)") {
		t.Fatalf("both said beside the status:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "c@x") && strings.Contains(line, "login") {
			t.Fatalf("a month away adds nothing: %q", line)
		}
		if strings.HasPrefix(line, "│ # ") && strings.Join(cells(line), " ") != "# ID CLI ACCOUNT USAGE UNTIL CHECKED STATUS" {
			t.Fatalf("no new column: %q", line)
		}
	}
}

// A login's last week is said under the table, a week being the notice
// signing several accounts in again wants; the STATUS cell waits for its last
// three days, as Claude Code's own warning does.
func TestListNotesALoginInItsLastWeek(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	now := time.Now()
	end := now.Add(5*24*time.Hour + 12*time.Hour + 30*time.Minute)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "extra":{"refresh_token_expires_at":"%[2]d"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"},"quotaAt":%[1]d,
		 "extra":{"refresh_token_expires_at":"%[3]d"}}]}`,
		now.UnixMilli(), end.UnixMilli(), now.Add(8*24*time.Hour).UnixMilli()))
	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	note := fmt.Sprintf("claude/a@x: login ends in 5d 12h (%s); rota login 1 before then", end.UTC().Format(time.DateOnly))
	if !strings.Contains(out, note) {
		t.Fatalf("the note, five and a half days ahead:\n%s", out)
	}
	if strings.Contains(out, "b@x: login") || strings.Contains(out, "(login ends") {
		t.Fatalf("eight days away is not said, and STATUS waits for the last three:\n%s", out)
	}
}

// An account marked dead by a rota that kept no reason does not read as if
// nothing were known: the cell says that much.
func TestListSaysSoWhenNoReasonWasKept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"},"quotaAt":%d,"dead":true}]}`, time.Now().UnixMilli()))
	out, _, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, "re-auth needed (") || !strings.Contains(out, "kept no reason") {
		t.Fatalf("the status cell has something to say:\n%s", out)
	}
}

// `list --json` is a map at the top, and a map is written with its keys
// sorted: the same store lists as the same bytes every time, so a script can
// diff two listings. Unsorted, "default" led the document in some runs and
// not in others.
func TestListJSONKeepsItsKeysInOneOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)
	p := filepath.Join(claude, "projects", "-x", "abcdef12-3456-7890-abcd-ef1234567890.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user","cwd":"/x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	want := []string{"accounts", "default", "instances", "sessions", "shared"}
	// "notes" says what this machine would not let the scan read (lsof can
	// fail under load), so it comes and goes; when it is there, it is in
	// its sorted place.
	withNotes := slices.Insert(slices.Clone(want), 3, "notes")
	// Several tries, because an unsorted map is only sometimes out of order.
	for i := range 16 {
		out, errOut, code := call(t, "--json", "list", "--short", "--sessions")
		if code != 0 {
			t.Fatalf("%d %q", code, errOut)
		}
		if got := topKeys(t, out); !slices.Equal(got, want) && !slices.Equal(got, withNotes) {
			t.Fatalf("try %d: top-level keys %q, want %q\n%s", i, got, want, out)
		}
	}
}

// topKeys is the names of a JSON object's members, in the order written.
func topKeys(t *testing.T, doc string) []string {
	t.Helper()
	dec := jsontext.NewDecoder(strings.NewReader(doc))
	if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '{' {
		t.Fatalf("not an object (%v):\n%s", err, doc)
	}
	var names []string
	for dec.PeekKind() != '}' {
		tok, err := dec.ReadToken()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tok.String())
		if err := dec.SkipValue(); err != nil {
			t.Fatal(err)
		}
	}
	return names
}

// An account rota holds no token for costs a listing no request: usage asked
// without a credential can only be refused, the refusal used to reach the
// person as a network error, and the request itself tells whoever watches the
// wire that rota runs here. The listing says why there is no reading instead,
// and an account that holds a token is read as ever.
func TestListAsksNothingForAnAccountWithoutAToken(t *testing.T) {
	var (
		mu    sync.Mutex
		asked []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprint(w, `{"five_hour":{"utilization":7}}`)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t2"}}]}`)

	out, errOut, code := call(t, "list")
	if code != 0 {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	mu.Lock()
	got := slices.Clone(asked)
	mu.Unlock()
	if len(got) != 1 || got[0] != "Bearer t2" {
		t.Fatalf("only the account with a token is asked, with its token: %q", got)
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "│") {
			continue
		}
		if strings.Contains(line, "b@x") && !strings.Contains(line, "5h 7%") {
			t.Fatalf("the account with a token has its reading: %q", line)
		}
		if strings.Contains(line, "a@x") && (!strings.Contains(line, "│ -") || !strings.Contains(line, "never")) {
			t.Fatalf("the account without one has none, as before: %q", line)
		}
	}
	if !strings.Contains(errOut, "claude/a@x") || !strings.Contains(errOut, "no access token") || strings.Contains(errOut, "http") {
		t.Fatalf("the warning says there is no access token, not what a network did: %q", errOut)
	}

	out, _, code = call(t, "--json", "list", "--refresh")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	mu.Lock()
	got = slices.Clone(asked)
	mu.Unlock()
	if len(got) != 2 || got[1] != "Bearer t2" {
		t.Fatalf("a forced reading asks for the token's account alone: %q", got)
	}
	if !strings.Contains(out, `"warnings"`) || !strings.Contains(out, "no access token") {
		t.Fatalf("json warns the same way:\n%s", out)
	}
}

// A usage endpoint that answers 429 with a Retry-After is left alone until
// then by every listing after it, forced or not, each saying why its reading
// is old; the listing itself goes on and succeeds, the account still in the
// rotation, and --json says when usage is asked again.
func TestListLeavesARateLimitedUsageEndpointAloneAndSaysSo(t *testing.T) {
	var (
		mu    sync.Mutex
		asked int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked++
		mu.Unlock()
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"type":"rate_limit_error","message":"Rate limited"}}`)
	}))
	t.Cleanup(srv.Close)
	old := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = old })
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	checked := time.Now().Add(-20 * time.Minute).UnixMilli()
	writeStore(t, home, fmt.Sprintf(`{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t1"},
		 "quota":{"windows":[{"name":"5h","percent":30,"primary":true}]},"quotaAt":%d}]}`, checked))
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return asked
	}

	for i, args := range [][]string{{"list"}, {"list", "--refresh"}, {"list"}} {
		out, errOut, code := call(t, args...)
		if code != 0 || count() != 1 {
			t.Fatalf("%v: exit %d, asked %d\n%s%s", args, code, count(), out, errOut)
		}
		if !strings.Contains(out, "5h 30%") || !strings.Contains(out, "20m ago") || !strings.Contains(out, "uses claude/a@x") {
			t.Fatalf("%v: the old reading, its age, and the account still first in line:\n%s", args, out)
		}
		if !strings.Contains(errOut, "claude/a@x: usage is not asked again until") || !strings.Contains(errOut, "asked rota to wait (http 429)") ||
			!strings.Contains(errOut, "the reading shown was taken at") || strings.Contains(errOut, "rate_limit_error") {
			t.Fatalf("%v (%d): one line saying why, without the provider's body: %q", args, i, errOut)
		}
	}

	out, _, code := call(t, "--json", "list")
	if code != 0 || count() != 1 {
		t.Fatalf("exit %d, asked %d: %s", code, count(), out)
	}
	var doc struct {
		Accounts []struct {
			UsageWaitUntil string `json:"usage_wait_until"`
			CheckedAgo     string `json:"checkedAgo"`
		} `json:"accounts"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	until, err := time.Parse(time.RFC3339, doc.Accounts[0].UsageWaitUntil)
	if err != nil || time.Until(until) < 9*time.Minute || time.Until(until) > 10*time.Minute {
		t.Fatalf("usage_wait_until is about ten minutes off: %q %v", doc.Accounts[0].UsageWaitUntil, err)
	}
	if doc.Accounts[0].CheckedAgo != "20m ago" || len(doc.Warnings) != 1 || !strings.Contains(doc.Warnings[0], "asked rota to wait") {
		t.Fatalf("%+v", doc)
	}
}
