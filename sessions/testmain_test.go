package sessions

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	rota "github.com/professor93/rota/lib"
)

// These tests open stores and choose among accounts, which can refresh a
// token or read usage and read a claude account's home. Nothing here reaches
// anything real: no provider call a test did not stand in for reaches the
// real one — every endpoint of every provider points at an address nothing
// answers on, so a made-up token never leaves the machine — no keychain or
// Claude Code daemon of anybody's is asked, and the person's own home and
// Claude Code directory are temporary ones.
func TestMain(m *testing.M) {
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
	claudecode.StandIn()
	person, err := os.MkdirTemp("", "rota-person")
	if err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Setenv("HOME", person)
	os.Setenv("USERPROFILE", person)
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	code := m.Run()
	os.RemoveAll(person)
	os.RemoveAll(dir)
	os.Exit(code)
}
