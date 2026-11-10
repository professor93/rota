package rotation

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// testHome and testClaudeDir are the home and the Claude Code directory
// every test here runs in, made by TestMain.
var testHome, testClaudeDir string

// These tests open stores and choose among accounts, which can refresh a
// token or read usage and read a claude account's home. Nothing here reaches
// anything real: no provider call a test did not stand in for reaches the
// real one — every endpoint of every provider points at an address nothing
// answers on, so a made-up token never leaves the machine — no keychain or
// Claude Code daemon of anybody's is asked, the person's own home and
// Claude Code directory are temporary ones, and every directory holding a
// real vendor CLI is left out of PATH, so a test that starts a CLI without
// installing a fake finds none, never the real one.
//
// The person's Anthropic profiles are not read either: a launch looks for
// the active one by ANTHROPIC_CONFIG_DIR and XDG_CONFIG_HOME before it
// looks in the home.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.ClaudeModelsEndpoint = u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
	claudecode.StandIn()
	var err error
	if testHome, err = os.MkdirTemp("", "rota-person"); err != nil {
		panic(err)
	}
	if testClaudeDir, err = os.MkdirTemp("", "rota-claude"); err != nil {
		panic(err)
	}
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Unsetenv("ANTHROPIC_CONFIG_DIR")
	os.Unsetenv("XDG_CONFIG_HOME")
	os.Setenv("HOME", testHome)
	os.Setenv("USERPROFILE", testHome)
	os.Setenv("CLAUDE_CONFIG_DIR", testClaudeDir)
	code := m.Run()
	os.RemoveAll(testHome)
	os.RemoveAll(testClaudeDir)
	os.Exit(code)
}

// No test here can start a real vendor CLI: TestMain left every directory
// holding one out of PATH.
func TestNoVendorCLIIsOnPath(t *testing.T) { fakecli.NoneOnPath(t) }

// The home every test here sees is TestMain's temporary one, and so is the
// Claude Code directory: never the person's who runs them.
func TestTheHomeIsATemporaryOne(t *testing.T) {
	if home, err := os.UserHomeDir(); err != nil || home != testHome {
		t.Errorf("the home is %q (%v), want TestMain's %q", home, err, testHome)
	}
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != testClaudeDir {
		t.Errorf("CLAUDE_CONFIG_DIR is %q, want TestMain's %q", dir, testClaudeDir)
	}
	for _, name := range []string{"ROTA_CLAUDE_HOME", "ROTA_ACCOUNT_ID", "ANTHROPIC_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		if v, set := os.LookupEnv(name); set {
			t.Errorf("%s is set, to %q", name, v)
		}
	}
}
