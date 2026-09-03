package main

import (
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

func mustWd(t *testing.T) string {
	t.Helper()
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
