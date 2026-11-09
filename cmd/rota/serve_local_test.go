package main

import (
	"strings"
	"testing"
)

// localToken is a server file whose one token is called local.
var localToken = "[[tokens]]\nname = \"local\"\nrole = \"watch\"\nsha256 = \"" + strings.Repeat("ab", 32) + "\"\n"

// Foreground rota serve has no control socket, so no principal called
// local, and a file whose user or token has that name serves there as it
// did before the daemon.
func TestForegroundServeTakesAPrincipalCalledLocal(t *testing.T) {
	writeConfig(t, localToken)
	t.Setenv("ROTA_TOKEN", "t")
	out, errb, code := call(t, "serve", "--print-config")
	if code != 0 {
		t.Fatalf("foreground serve with a token called local: exit %d %q", code, errb)
	}
	if got := lineFor(t, out, "name"); got != `name = "local" # file` {
		t.Fatalf("the token is %q", got)
	}
}
