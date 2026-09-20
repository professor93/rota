package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// `rota set` is how an account is told where it belongs. It refuses the
// two settings that go wrong quietly rather than storing them and failing
// on every run afterwards.
func TestProjectThroughTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	t.Setenv("PATH", t.TempDir()) // no vendor CLI anywhere

	out, _, code := call(t, "login", "t-cli-fake")
	if code != 0 {
		t.Fatalf("auth: %d %q", code, out)
	}
	m := regexp.MustCompile(`rota login ([0-9a-f]+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no login id in %q", out)
	}
	if out, _, code := call(t, "login", m[1], "sk-test-key"); code != 0 {
		t.Fatalf("finish: %d %q", code, out)
	}

	// Nothing set: it says so rather than pretending.
	if out, _, code := call(t, "set", "1"); code != 0 || !strings.Contains(out, "wherever rota is run from") {
		t.Fatalf("unset: %d %q", code, out)
	}

	project := t.TempDir()
	config := t.TempDir()
	if out, _, code := call(t, "set", "1", "--cwd", project, "--config", config); code != 0 ||
		!strings.Contains(out, project) || !strings.Contains(out, config) {
		t.Fatalf("set: %d %q", code, out)
	}
	// And it stays set, in the store rather than in the process.
	if out, _, code := call(t, "set", "1"); code != 0 || !strings.Contains(out, project) {
		t.Fatalf("reread: %d %q", code, out)
	}

	// A credential file does not belong in a repository.
	if _, err, code := call(t, "set", "1", "--config", project); code == 0 ||
		!strings.Contains(err, "credential") {
		t.Fatalf("same directory: %d %q", code, err)
	}
	// Nor in rota's own directories: another account's home is where that
	// account's credential is staged, and what removing it deletes.
	if _, err, code := call(t, "set", "1", "--config", filepath.Join(home, "homes", "claude-2")); code != 2 ||
		!strings.Contains(err, "rota's own") {
		t.Fatalf("a sibling's home: %d %q", code, err)
	}
	// The refusal must not have half-applied.
	if out, _, code := call(t, "set", "1"); code != 0 || !strings.Contains(out, config) {
		t.Fatalf("the rejected change was kept: %d %q", code, out)
	}

	// A relative path is made absolute rather than refused: the shell that
	// typed it is the one that meant it.
	if out, _, code := call(t, "set", "1", "--cwd", "."); code != 0 || !strings.Contains(out, filepath.Base(mustWd(t))) {
		t.Fatalf("relative: %d %q", code, out)
	}

	if out, _, code := call(t, "set", "1", "--clear"); code != 0 ||
		!strings.Contains(out, "wherever rota is run from") {
		t.Fatalf("clear: %d %q", code, out)
	}
}

// Where a claude account's conversations live is set in the same write as
// everything else about it, and takes the three answers there are: shared
// with your own Claude Code directory, the account's own, or a folder.
func TestSessionsThroughTheCLI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"token":{"accessToken":"t"}},
		{"id":2,"provider":"codex","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)

	// Unset is shared, and says so rather than saying nothing.
	if out, _, code := call(t, "set", "1"); code != 0 || !strings.Contains(out, "shared with your own") {
		t.Fatalf("unset: %d %q", code, out)
	}
	if out, _, code := call(t, "set", "1", "--sessions", "own"); code != 0 ||
		!strings.Contains(out, "this account's own") {
		t.Fatalf("own: %d %q", code, out)
	}
	folder := t.TempDir()
	if out, _, code := call(t, "set", "1", "--sessions", folder); code != 0 || !strings.Contains(out, folder) {
		t.Fatalf("a folder: %d %q", code, out)
	}
	// And it stays set, in the store rather than in the process.
	if out, _, code := call(t, "set", "1"); code != 0 || !strings.Contains(out, folder) {
		t.Fatalf("reread: %d %q", code, out)
	}
	// A relative path is made absolute exactly as --config is.
	if out, _, code := call(t, "set", "1", "--sessions", "."); code != 0 ||
		!strings.Contains(out, filepath.Base(mustWd(t))) {
		t.Fatalf("relative: %d %q", code, out)
	}
	// Nor may it be one of rota's own directories.
	if _, err, code := call(t, "set", "1", "--sessions", filepath.Join(home, "homes", "claude-1")); code == 0 ||
		!strings.Contains(err, "rota's own") {
		t.Fatalf("rota's own: %d %q", code, err)
	}

	// A caller scripting this reads it back whole.
	// The folder is looked for as JSON writes it: on Windows every separator
	// in it is escaped, and the path as the shell spells it is not in there.
	out, _, code := call(t, "--json", "set", "1", "--sessions", folder)
	if code != 0 || !strings.Contains(out, `"sessions"`) || !strings.Contains(out, strings.ReplaceAll(folder, `\`, `\\`)) {
		t.Fatalf("json: %d %q", code, out)
	}

	// shared is the word for the default, and --clear forgets it too.
	if out, _, code := call(t, "set", "1", "--sessions", "shared"); code != 0 ||
		!strings.Contains(out, "shared with your own") {
		t.Fatalf("shared: %d %q", code, out)
	}
	if out, _, code := call(t, "set", "1", "--sessions", "own"); code != 0 {
		t.Fatalf("own again: %d %q", code, out)
	}
	if out, _, code := call(t, "set", "1", "--clear"); code != 0 ||
		!strings.Contains(out, "shared with your own") {
		t.Fatalf("clear: %d %q", code, out)
	}

	// No other CLI keeps its conversations where rota could move them, so
	// the setting is refused by name rather than stored and disobeyed.
	if _, err, code := call(t, "set", "2", "--sessions", "own"); code == 0 ||
		!strings.Contains(err, "Claude Code") {
		t.Fatalf("codex: %d %q", code, err)
	}
	if out, _, code := call(t, "set", "2"); code != 0 || strings.Contains(out, "sessions") {
		t.Fatalf("and is not offered for one: %d %q", code, out)
	}
}

// An account keeping its conversations to itself keeps them in the home
// rota made for it, and removing the account deletes that home. That is
// right — they are nobody else's to read — but it is worth saying out loud
// rather than leaving somebody to find out afterwards.
func TestRemovingAnAccountSaysWhenItsOwnConversationsGoWithIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ROTA_HOME", home)
	writeStore(t, home, `{"ordered":true,"nextId":3,"accounts":[
		{"id":1,"provider":"claude","email":"a@x","order":1,"sessions":"own","token":{"accessToken":"t"}},
		{"id":2,"provider":"claude","email":"b@x","order":2,"token":{"accessToken":"t"}}]}`)
	if err := os.MkdirAll(filepath.Join(home, "homes", "claude-1", "projects", "-tmp-x"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The one sharing has links rather than conversations, so it loses none.
	if err := os.MkdirAll(filepath.Join(home, "homes", "claude-2"), 0o700); err != nil {
		t.Fatal(err)
	}

	out, _, code := call(t, "remove", "1")
	if code != 0 || !strings.Contains(out, "went with it") {
		t.Fatalf("own: %d %q", code, out)
	}
	if out, _, code := call(t, "remove", "2"); code != 0 || strings.Contains(out, "went with it") {
		t.Fatalf("shared: %d %q", code, out)
	}
}

func mustWd(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
