package notify

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
	rota "github.com/professor93/rota/lib"
)

// TestMain keeps every test here off the machine it runs on: a command sink
// runs a fake from internal/fakecli and nothing else, no keychain and no
// provider is reached, Telegram is an address nothing answers on until a
// test stands one up, and the person's own homes are a temporary one.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	claudecode.StandIn()
	const u = claudecode.Unreachable
	rota.ClaudeEndpoints.Authorize, rota.ClaudeEndpoints.Token, rota.ClaudeEndpoints.Profile, rota.ClaudeEndpoints.Usage = u, u, u, u
	rota.ClaudeModelsEndpoint = u
	rota.CodexEndpoints.Authorize, rota.CodexEndpoints.Token = u, u
	// A closed loopback port: a sink a test did not point at a fake of its
	// own fails here rather than reaching Telegram.
	telegramAPI = "http://127.0.0.1:1"

	person, err := os.MkdirTemp("", "rota-person")
	if err != nil {
		panic(err)
	}
	claude, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	// No program is found on PATH: a command sink looks its program up
	// there, and a test that runs one sets PATH to a directory of fakes.
	bare, err := os.MkdirTemp("", "rota-path")
	if err != nil {
		panic(err)
	}
	for _, name := range []string{"ROTA_CLAUDE_HOME", "ROTA_ACCOUNT_ID", "ANTHROPIC_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		os.Unsetenv(name)
	}
	os.Setenv("HOME", person)
	os.Setenv("USERPROFILE", person)
	os.Setenv("CLAUDE_CONFIG_DIR", claude)
	os.Setenv("PATH", bare)
	code := m.Run()
	os.RemoveAll(person)
	os.RemoveAll(claude)
	os.RemoveAll(bare)
	os.Exit(code)
}
