package rota

import (
	"bytes"
	"context"
	"github.com/professor93/rota/internal/fakecli"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hermetic gives a claude run a throwaway config directory, so nothing from
// the person's shared home — identity above all — reaches the model's
// context. Billing always followed the token; this makes the *context*
// follow it too. The directory is removed when the run ends.
func TestHermeticIsolatesTheConfigDirectoryAndCleansItUp(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r // ScratchDir is symlink-resolved at check time; compare resolved
	}
	bin := fakecli.Install(t, t.TempDir(), "envcli", fakecli.Spec{KeepStdin: true, Stdout: []string{
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"DIR={{env:CLAUDE_CONFIG_DIR}}","num_turns":1}`,
	}})
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	given := &Command{Bin: bin, Env: []string{"FAKE=1"}, BaseEnv: []string{"PATH=/usr/bin:/bin", "CLAUDE_CONFIG_DIR=/Users/me/.claude"}}
	var out bytes.Buffer
	res, err := Run(context.Background(), a, "", given,
		Spec{Prompt: "p", Hermetic: true, ScratchDir: dir, flavorOverride: "claude"}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	_, got, ok := strings.Cut(res.Result, "DIR=")
	if !ok || got == "" || got == "/Users/me/.claude" {
		t.Fatalf("the child must see a private config dir, not the shared one: %q", res.Result)
	}
	if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
		t.Fatalf("the throwaway dir obeys ScratchDir: %q", got)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("the throwaway dir must be gone when the run ends: %v", err)
	}
}

// Hermetic is claude vocabulary; a provider with no shared home to hide is
// refused by name rather than silently ignored.
func TestHermeticIsClaudeVocabulary(t *testing.T) {
	if _, err := specArgv(Spec{Prompt: "p", Hermetic: true}, "grok", nil); err == nil {
		t.Fatal("grok must refuse a field it cannot honour")
	}
}

// An account with a config directory of its own already carries
// CLAUDE_CONFIG_DIR in its command; hermetic must replace it, not add a
// second. Node takes the first of two, which would be the shared one.
func TestHermeticReplacesAnExistingConfigDir(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	bin := fakecli.Install(t, t.TempDir(), "envcli", fakecli.Spec{KeepStdin: true, Stdout: []string{
		`{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"DIR={{env:CLAUDE_CONFIG_DIR}}","num_turns":1}`,
	}})
	a := &Account{ID: 1, Provider: "claude"}
	a.Token.Access = "tok"
	given := &Command{Bin: bin, Env: []string{"CLAUDE_CONFIG_DIR=/x", "FAKE=1"}, BaseEnv: []string{"PATH=/usr/bin:/bin"}}
	var out bytes.Buffer
	res, err := Run(context.Background(), a, "", given,
		Spec{Prompt: "p", Hermetic: true, ScratchDir: dir, flavorOverride: "claude"}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	_, got, _ := strings.Cut(res.Result, "DIR=")
	if !strings.HasPrefix(got, dir+string(filepath.Separator)) {
		t.Fatalf("the child must see the throwaway dir, not the account's: %q", res.Result)
	}
	// The command hermetic builds carries exactly one, and Environ keeps it so.
	env := Environ([]string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/shared"}, hermeticCommand(given, "/h"))
	n := 0
	for _, e := range env {
		if k, v, _ := strings.Cut(e, "="); k == "CLAUDE_CONFIG_DIR" {
			n++
			if v != "/h" {
				t.Fatalf("the hermetic dir must be the one that survives: %v", env)
			}
		}
	}
	if n != 1 {
		t.Fatalf("exactly one CLAUDE_CONFIG_DIR, got %d in %v", n, env)
	}
	if strings.Join(given.Env, ",") != "CLAUDE_CONFIG_DIR=/x,FAKE=1" {
		t.Fatalf("the caller's command must not be rewritten: %v", given.Env)
	}
}
