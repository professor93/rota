package store

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/claudecode"
	"github.com/professor93/rota/internal/fakecli"
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
	dir, err := os.MkdirTemp("", "rota-claude")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir)
	claudecode.StandIn()
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
