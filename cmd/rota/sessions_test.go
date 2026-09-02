package main

import (
	"github.com/professor93/rota/internal/fakecli"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `rota list --sessions` says what the CLIs are doing: which are running, and
// which conversations could be picked up again.
//
// The home is pointed somewhere empty so the test describes what it planted
// rather than whatever this machine happens to have open.
func TestListSessionsShowsWhatIsRunningAndWhatCanResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)

	// One conversation, and one editor holding a workspace open.
	transcript := filepath.Join(claude, "projects", "-Users-me-src-api", "abcdef12-0000-0000-0000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","cwd":"/Users/me/src/api"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(claude, "ide", "1234.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	const secret = "mo_averysecretlookingeditortoken"
	body := `{"workspaceFolders":["/Users/me/src/api"],"pid":` + pid(t) + `,"ideName":"GoLand","authToken":"` + secret + `"}`
	if err := os.WriteFile(lock, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, code := call(t, "list", "--short", "--sessions")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	for _, want := range []string{"Running instances:", "GoLand", "Sessions:", "abcdef12", "/Users/me/src/api"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// The editor's own credential must never reach a terminal.
	if strings.Contains(out, secret) || strings.Contains(out, "authToken") {
		t.Fatalf("a token from a lock file leaked into the listing:\n%s", out)
	}
	// A claude account with no --config of its own reads the person's home,
	// so the conversation there belongs to nobody and is said to be shared.
	if !strings.Contains(out, "shared") {
		t.Fatalf("an unowned conversation must say so:\n%s", out)
	}

	// Without the flag, none of it is looked at.
	if out, _, code := call(t, "list", "--short"); code != 0 || strings.Contains(out, "Running instances:") {
		t.Fatalf("sessions are opt-in: %d %q", code, out)
	}
}

// The same in JSON, because a caller that scripts this needs the whole ids
// and the counts rather than the columns.
func TestListSessionsInJSONCarriesBothSections(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	claude := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	writeStore(t, home, `{"ordered":true,"nextId":2,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}}]}`)
	full := "abcdef12-3456-7890-abcd-ef1234567890"
	p := filepath.Join(claude, "projects", "-x", full+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"type":"user","cwd":"/x"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, code := call(t, "--json", "list", "--short", "--sessions")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	// The whole id, not the shortened one the columns show.
	if !strings.Contains(out, full) {
		t.Fatalf("json must carry the id --resume takes:\n%s", out)
	}
	for _, want := range []string{`"sessions"`, `"shared"`, `"instances"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
}

// pid is this process, which is certainly alive, as a decimal string.
func pid(t *testing.T) string {
	t.Helper()
	n, digits := os.Getpid(), ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// The point of the registry: a running CLI is traceable to the account whose
// quota is paying for it. Nothing on disk says that by default, because every
// Claude Code account with no --config reads the same ~/.claude.
//
// The handover is where it matters most. execve replaces rota with the CLI and
// keeps the process id, so the entry written beforehand is what describes the
// instance for as long as it runs -- and there is no code path afterwards that
// could write one.
func TestAHandedOverRunIsListedUnderItsAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":2,"provider":"claude","email":"payer@x","order":1,"token":{"accessToken":"t"}}]}`)
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{})
	t.Setenv("PATH", bin)

	handover(t)
	if _, errOut, code := call(t, "run", "2"); code != 0 {
		t.Fatalf("handover: %d %q", code, errOut)
	}

	out, _, code := call(t, "list", "--short", "--sessions")
	if code != 0 {
		t.Fatalf("%d %q", code, out)
	}
	if !strings.Contains(out, "#2 payer@x") {
		t.Fatalf("the running CLI must name the account paying for it:\n%s", out)
	}
}
