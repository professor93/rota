package rota

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// The test binary doubles as every fake vendor CLI the tests install.
//
// And no provider call a test did not stand in for reaches the real one:
// every endpoint of every provider points at an address nothing answers on,
// until a test points it at a fake of its own.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	const unreachable = "http://127.0.0.1:1/unreachable"
	ClaudeEndpoints.Authorize, ClaudeEndpoints.Token, ClaudeEndpoints.Profile, ClaudeEndpoints.Usage =
		unreachable, unreachable, unreachable, unreachable
	CodexEndpoints.Authorize, CodexEndpoints.Token = unreachable, unreachable
	os.Exit(m.Run())
}
