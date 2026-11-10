//go:build linux || darwin

package pty

import (
	"os"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// The shells these tests start on a terminal look programs up on PATH, and
// every directory holding a real vendor CLI is left out of it: nothing
// typed at one can start the real thing.
func TestMain(m *testing.M) {
	fakecli.Maybe()
	os.Exit(m.Run())
}

// No test here can start a real vendor CLI: TestMain left every directory
// holding one out of PATH.
func TestNoVendorCLIIsOnPath(t *testing.T) { fakecli.NoneOnPath(t) }
