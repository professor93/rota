//go:build !linux && !darwin

package main

import (
	"testing"

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
