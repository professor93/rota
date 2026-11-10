package main

import (
	"os"
	"path/filepath"
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

// A user who signs in by key alone is not among the users the server is
// given, but is a person all the same: a daemon's file that calls one local
// is refused, naming the entry, as one with a password would be.
func TestADaemonRefusesAKeyOnlyUserCalledLocal(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "server.toml")
	body := "[[users]]\nname = \"me\"\nrole = \"watch\"\nssh_keys = [\"" + keyOnlyLine + "\"]\n" +
		"[[users]]\nname = \"local\"\nrole = \"control\"\nssh_keys = [\"" + keyOnlyLine2 + "\"]\n"
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := daemonConfig(conf, t.TempDir())
	want := `[[users]] "local": the name "local" is reserved for the control socket's own principal; choose another`
	if err == nil || err.Error() != want {
		t.Fatalf("a daemon's key-only user called local answers %v, want %q", err, want)
	}
}

// keyOnlyLine and keyOnlyLine2 are two ed25519 public keys as ssh_keys
// lines: the bytes 0..31 and 32..63.
const (
	keyOnlyLine  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f"
	keyOnlyLine2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICAhIiMkJSYnKCkqKywtLi8wMTIzNDU2Nzg5Ojs8PT4/"
)
