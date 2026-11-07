//go:build !linux && !darwin

package main

import (
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// There is no daemon here: start, stop, status, install and uninstall each
// refuse in the sentence every held-session command uses, and start no
// process.
func TestTheDaemonIsRefusedHere(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, verb := range []string{"start", "stop", "status", "install", "uninstall"} {
		out, errb, code := call(t, "serve", verb)
		if code != 1 || out != "" || !strings.Contains(errb, holdwire.ErrPlatform.Error()) {
			t.Fatalf("serve %s: exit %d %q %q", verb, code, out, errb)
		}
	}
}
