package store

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// The test binary doubles as every fake vendor CLI the tests install.
//
// It also runs in a Claude Code configuration directory of its own. Every
// claude run mirrors the directory this process is in, and the one belonging
// to whoever is running the tests is neither predictable nor anybody's to
// touch: reading it would make these tests depend on a stranger's files.
//
// And it never reaches a real keychain or a real Claude Code daemon: the
// keychain holds nothing unless a test says it does, and stopping a daemon
// fails unless a test stands in for it.
func TestMain(m *testing.M) {
	fakecli.Maybe()
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
	dir, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	claudecode.StandIn()
	// And no provider call a test did not stand in for reaches the real one:
	// every endpoint of every provider, whatever account a test makes.
	offline()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// offline points every endpoint of every provider at an address nothing
// answers on, so a made-up token never leaves the machine; a test that wants
// an answer points the one it asks at a fake of its own.
func offline() {
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
}
