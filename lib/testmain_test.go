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

// The test binary doubles as every fake vendor CLI the tests install. As
// itself it runs with every directory holding a real one left out of PATH,
// so a test that forgets its fake finds none, never the real one.
//
// And no provider call a test did not stand in for reaches the real one:
// every endpoint of every provider points at an address nothing answers on,
// until a test points it at a fake of its own.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	builtins = Providers()
	const unreachable = "http://127.0.0.1:1/unreachable"
	ClaudeEndpoints.Authorize, ClaudeEndpoints.Token, ClaudeEndpoints.Profile, ClaudeEndpoints.Usage =
		unreachable, unreachable, unreachable, unreachable
	ClaudeModelsEndpoint = unreachable
	CodexEndpoints.Authorize, CodexEndpoints.Token = unreachable, unreachable
	os.Exit(m.Run())
}

// No test here can start a real vendor CLI: TestMain left every directory
// holding one out of PATH.
func TestNoVendorCLIIsOnPath(t *testing.T) { fakecli.NoneOnPath(t) }

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
