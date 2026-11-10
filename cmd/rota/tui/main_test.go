package tui

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// TestMain doubles as the fake vendor CLIs a test installs with
// fakecli.Install, and keeps every test here away from the person running
// them: their keychain, their Claude Code daemon, their providers, their
// home and whatever a CLI would find on their PATH.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	// No keychain and no Claude Code daemon of anybody's is reached from
	// here, nor from a run the in-process API starts.
	claudecode.StandIn()
	// No provider call a test did not stand in for reaches the real one:
	// every endpoint of every provider, whatever account a test makes.
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.ClaudeModelsEndpoint = u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
	// Only a fake a test installs is ever found: PATH starts empty, and a
	// test that installs one puts its directory first.
	bin, err := os.MkdirTemp("", "rota-tui-path")
	if err != nil {
		panic(err)
	}
	os.Setenv("PATH", bin)
	// Nor does anything here reach the person's own home: ~/.claude is what
	// rota falls back to when CLAUDE_CONFIG_DIR names an account's home, and
	// a test that fell back would read a stranger's files.
	person, err := os.MkdirTemp("", "rota-person")
	if err != nil {
		panic(err)
	}
	// Whose Claude Code directory is the person's is read from variables a
	// rota that launched this one would have set; a test answers from its
	// own environment alone.
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Setenv("HOME", person)
	os.Setenv("USERPROFILE", person)
	// Nor the person's Anthropic profiles: a claude launch looks for the
	// active one by these two before it looks in the home.
	os.Unsetenv("ANTHROPIC_CONFIG_DIR")
	os.Unsetenv("XDG_CONFIG_HOME")
	dir, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(person)
	os.RemoveAll(bin)
	os.Exit(code)
}
