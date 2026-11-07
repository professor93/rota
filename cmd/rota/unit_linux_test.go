//go:build linux

package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Without lingering, systemd starts a user's units at login and stops them
// at logout, so install says how to have it start the daemon at boot and
// keep it after logout — once, until it is so.
func TestInstallSaysHowToLinger(t *testing.T) {
	newUnitRig(t)
	old := lingerDir
	lingerDir = t.TempDir()
	t.Cleanup(func() { lingerDir = old })
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if name == "" {
		t.Skip("who this user is cannot be told")
	}
	hint := `run "loginctl enable-linger ` + name + `" so the daemon starts at boot and outlives your login`
	_, errb, code := call(t, "serve", "install")
	if code != 0 || strings.TrimSpace(errb) != hint {
		t.Fatalf("install: exit %d %q, want %q", code, errb, hint)
	}
	if err := os.WriteFile(filepath.Join(lingerDir, name), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errb, code := call(t, "serve", "install"); code != 0 || errb != "" {
		t.Fatalf("install with lingering: exit %d %q", code, errb)
	}
}
