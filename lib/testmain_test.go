package rota

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// builtins are the providers this package registers by itself, read before
// any test adds one of its own.
var builtins []string

// testHome and testClaudeDir are the home and the Claude Code directory
// every test here runs in, made by TestMain.
var testHome, testClaudeDir string

// The test binary doubles as every fake vendor CLI the tests install. As
// itself it runs with every directory holding a real one left out of PATH,
// so a test that forgets its fake finds none, never the real one.
//
// And no provider call a test did not stand in for reaches the real one:
// every endpoint of every provider points at an address nothing answers on,
// until a test points it at a fake of its own.
//
// Nor does anything here reach the person's own home or Claude Code
// directory: a launch hands its CLI this process's environment, and a fake
// that reads or writes where Claude Code keeps its files would otherwise do
// so in a stranger's.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	builtins = Providers()
	const unreachable = "http://127.0.0.1:1/unreachable"
	ClaudeEndpoints.Authorize, ClaudeEndpoints.Token, ClaudeEndpoints.Profile, ClaudeEndpoints.Usage =
		unreachable, unreachable, unreachable, unreachable
	ClaudeModelsEndpoint = unreachable
	CodexEndpoints.Authorize, CodexEndpoints.Token = unreachable, unreachable
	var err error
	if testHome, err = os.MkdirTemp("", "rota-person"); err != nil {
		panic(err)
	}
	if testClaudeDir, err = os.MkdirTemp("", "rota-claude"); err != nil {
		panic(err)
	}
	// Whose Claude Code directory is the person's is read from variables a
	// rota that launched this one would have set; a test answers from its
	// own environment alone.
	os.Unsetenv("ROTA_CLAUDE_HOME")
	os.Unsetenv("ROTA_ACCOUNT_ID")
	os.Setenv("HOME", testHome)
	os.Setenv("USERPROFILE", testHome)
	// Nor the person's Anthropic profiles: a claude launch looks for the
	// active one by these two before it looks in the home.
	os.Unsetenv("ANTHROPIC_CONFIG_DIR")
	os.Unsetenv("XDG_CONFIG_HOME")
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

// Every CLI a provider built into rota launches is one Maybe keeps off PATH:
// a provider added without its CLI in fakecli's list would be a vendor CLI a
// test elsewhere could start for real.
func TestEveryCLIABuiltinProviderLaunchesIsKeptOffPath(t *testing.T) {
	if len(builtins) == 0 {
		t.Fatal("no provider registered itself")
	}
	for _, name := range builtins {
		p, err := Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := p.Launch(&Account{Provider: name, Token: Token{Access: "t", Refresh: "r"},
			Extra: map[string]string{"id_token": "i"}, Delegated: true}, t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if bin := strings.TrimSuffix(filepath.Base(cmd.Bin), ".exe"); !slices.Contains(fakecli.Vendors(), bin) {
			t.Errorf("provider %s launches %s, which fakecli.Maybe leaves on PATH", name, bin)
		}
	}
}
