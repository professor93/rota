package rota

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An account kept for one project should not have to be told which project
// on every request.

func TestAnAccountLendsItsDirectoryToARequestThatNamesNone(t *testing.T) {
	a := &Account{ID: 1, Provider: "claude", Cwd: "/projects/foo"}
	if got := (Spec{Prompt: "p"}).For(a); got.Cwd != "/projects/foo" {
		t.Fatalf("cwd: %q", got.Cwd)
	}
}

func TestARequestThatNamesADirectoryKeepsIt(t *testing.T) {
	a := &Account{ID: 1, Provider: "claude", Cwd: "/projects/foo"}
	if got := (Spec{Prompt: "p", Cwd: "/somewhere/else"}).For(a); got.Cwd != "/somewhere/else" {
		t.Fatalf("cwd: %q", got.Cwd)
	}
}

func TestForLeavesTheOriginalAlone(t *testing.T) {
	a := &Account{ID: 1, Provider: "claude", Cwd: "/projects/foo"}
	spec := Spec{Prompt: "p"}
	_ = spec.For(a)
	if spec.Cwd != "" {
		t.Fatalf("For must return a copy, not edit its receiver: %q", spec.Cwd)
	}
}

// Claude Code reads memory, skills and settings from its config directory.
// Left alone it is the person's own; an account given one of its own gets
// that project's instead.
func TestAClaudeAccountWithItsOwnConfigDirectoryIsToldWhereItIs(t *testing.T) {
	home := t.TempDir()

	plain := &Account{ID: 1, Provider: "claude", Token: Token{Access: "tok"}}
	cmd, err := Stage(plain, home)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(cmd.Env, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") }) {
		t.Fatalf("an account with no directory of its own must not move the CLI's: %v", cmd.Env)
	}

	own := &Account{ID: 2, Provider: "claude", Token: Token{Access: "tok"}, ConfigDir: home}
	cmd, err = Stage(own, home)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CONFIG_DIR="+home) {
		t.Fatalf("CLAUDE_CONFIG_DIR missing: %v", cmd.Env)
	}
}

// A relative config directory would resolve against whatever directory the
// process happened to start in, which for a server is not a place to keep
// credentials.
func TestAConfigDirectoryMustBeAbsolute(t *testing.T) {
	a := &Account{ID: 1, Provider: "claude", ConfigDir: "relative/path"}
	err := a.CheckProject()
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("got %v", err)
	}
	if err := (&Account{ID: 1, Provider: "claude", ConfigDir: filepath.Join(t.TempDir(), "c")}).CheckProject(); err != nil {
		t.Fatal(err)
	}
	if err := (&Account{ID: 1, Provider: "claude", Cwd: "also/relative"}).CheckProject(); err == nil {
		t.Fatal("a relative working directory is the same mistake")
	}
	if err := (&Account{ID: 1, Provider: "claude"}).CheckProject(); err != nil {
		t.Fatalf("neither set is fine: %v", err)
	}
}

// Credentials are staged in the config directory. The working directory is
// a project, often a repository someone will commit.
func TestAnAccountWillNotKeepItsCredentialsInItsProject(t *testing.T) {
	dir := t.TempDir()
	a := &Account{ID: 1, Provider: "codex", Cwd: dir, ConfigDir: dir}
	err := a.CheckProject()
	if err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("got %v", err)
	}
}
