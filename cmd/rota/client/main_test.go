package client

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// personHome is the HOME this test binary was started with, the person's
// own: kept only so a test can say nothing here reached it.
var personHome string

// TestMain plays a fake CLI when this binary was started as one: the
// terminals these tests open run the tty fake as claude. Otherwise it
// keeps every test away from the person running them, as cmd/rota's own
// TestMain does: no keychain and no Claude Code daemon, no provider
// endpoint, and a HOME and Claude Code directory of the tests' own, so a
// claude terminal's home mirrors a throwaway directory and never ~/.claude.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	claudecode.StandIn()
	offline()
	personHome, _ = os.UserHomeDir()
	person, err := os.MkdirTemp("", "rota-person")
	if err != nil {
		panic(err)
	}
	// Whose Claude Code directory is the person's is read from variables a
	// rota that launched this one would have set.
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Setenv("HOME", person)
	os.Setenv("USERPROFILE", person)
	// Nor the person's Anthropic profiles, which a claude launch looks for
	// by these before it looks in the home.
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
	os.Exit(code)
}

// offline points every endpoint of every provider at an address nothing
// answers on, so a made-up token never leaves the machine.
func offline() {
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.ClaudeModelsEndpoint = u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
}
