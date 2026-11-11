//go:build !linux && !darwin

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/professor93/rota/api"
)

// Where there is no pseudo-terminal there is no SSH server: an attach would
// have no terminal to give the person.
func TestSSHOptionsRefuseWindows(t *testing.T) {
	cfg := api.DefaultConfig()
	cfg.SSH.Enabled = true
	_, err := sshOptions(cfg, t.TempDir(), t.TempDir(), nil, nil)
	if err == nil || err.Error() != "the SSH server runs on linux and macOS" {
		t.Fatalf("sshOptions here: %v", err)
	}
	if _, err := sshwireServe(cfg, t.TempDir(), nil, nil); err == nil || err.Error() != "the SSH server runs on linux and macOS" {
		t.Fatalf("foreground serve's SSH here: %v", err)
	}
}

// Foreground serve with [ssh] enabled is refused here in the platform's
// sentence, before anything else is asked of the file, [routes] terminal
// included, and before it says it serves.
func TestForegroundSSHRefusedHereFirst(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	file := "[ssh]\nenabled = true\nlisten = \"127.0.0.1:2222\"\n\n" +
		"[[users]]\nname = \"me\"\nrole = \"control\"\nssh_keys = [\"" + key + "\"]\n"
	for name, extra := range map[string]string{"without terminals": "", "with terminals": "\n[routes]\nterminal = true\n"} {
		t.Run(name, func(t *testing.T) {
			addr := refusing(t)
			conf := filepath.Join(t.TempDir(), "server.toml")
			if err := os.WriteFile(conf, []byte(file+extra), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errb bytes.Buffer
			code := run([]string{"serve", addr, "--config", conf}, &out, &errb)
			if code != 2 || !strings.Contains(errb.String(), "the SSH server runs on linux and macOS") ||
				strings.Contains(errb.String(), "routes") || strings.Contains(errb.String(), "serving") {
				t.Fatalf("serve with [ssh] enabled here: exit %d %q %q", code, out.String(), errb.String())
			}
		})
	}
}
