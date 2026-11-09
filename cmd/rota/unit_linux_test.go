//go:build linux

package main

import (
	"errors"
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

// A disable that fails — no user bus, say — leaves the link enable made in
// default.target.wants; uninstall takes it away with the unit, so nothing
// is left naming a file that is gone, which no later uninstall could find.
// The error names each systemctl command that failed.
func TestAFailedDisableLeavesNoWantsLink(t *testing.T) {
	home := unitHome(t, true)
	store := filepath.Join(home, ".rota")
	recordUnits(t, nil)
	path, err := writeUnit("/u/rota", store, []string{"PATH=/p"}, false)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(path)
	wants := filepath.Join(filepath.Dir(path), "default.target.wants", name)
	if err := os.MkdirAll(filepath.Dir(wants), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, wants); err != nil {
		t.Fatal(err)
	}
	recordUnits(t, func(call string) error { return errors.New(call + ": Failed to connect to bus") })
	_, err = removeUnit(store, filepath.Join(store, "terminals"))
	for _, want := range []string{"systemctl --user disable --now " + name + ": Failed", "systemctl --user daemon-reload: Failed"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("removeUnit: %v, want %q", err, want)
		}
	}
	for _, p := range []string{path, wants} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", p, err)
		}
	}
}
