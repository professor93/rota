//go:build !linux && !darwin

package main

import (
	"strings"
	"testing"

	"github.com/professor93/rota/internal/holdwire"
)

// There is no daemon here: start, stop, status, install and uninstall each
// refuse in the sentence every held-session command uses, and start no
// process; so does run --detach, which hands the CLI to one.
func TestTheDaemonIsRefusedHere(t *testing.T) {
	t.Setenv("ROTA_HOME", t.TempDir())
	for _, args := range [][]string{{"serve", "start"}, {"serve", "stop"}, {"serve", "status"},
		{"serve", "install"}, {"serve", "uninstall"}, {"run", "1", "--detach"}} {
		out, errb, code := call(t, args...)
		if code != 1 || out != "" || !strings.Contains(errb, holdwire.ErrPlatform.Error()) {
			t.Fatalf("%s: exit %d %q %q", strings.Join(args, " "), code, out, errb)
		}
	}
}
