//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
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
// default.target.wants; uninstall, when no user manager runs to start the
// daemon again, takes it away with the unit, so nothing is left naming a
// file that is gone, which no later uninstall could find. The error names
// the systemctl command that failed, and no reload is asked of a manager
// that is not there.
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
	noManager(t)
	calls := recordUnits(t, func(call string) error { return errors.New(call + ": Failed to connect to bus") })
	_, err = removeUnit(store, filepath.Join(store, "terminals"))
	if want := "systemctl --user disable --now " + name + ": Failed to connect to bus"; err == nil || err.Error() != want {
		t.Fatalf("removeUnit: %v, want %q", err, want)
	}
	if want := []string{"systemctl --user disable --now " + name}; !slices.Equal(*calls, want) {
		t.Fatalf("asked %q, want %q", *calls, want)
	}
	for _, p := range []string{path, wants} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", p, err)
		}
	}
}

// noManager points where the user manager is looked for at empty
// directories, until the test ends: none runs.
func noManager(t *testing.T) {
	t.Helper()
	oldProc, oldCgroup := procDir, cgroupDir
	procDir, cgroupDir = t.TempDir(), t.TempDir()
	t.Cleanup(func() { procDir, cgroupDir = oldProc, oldCgroup })
}

// On Linux a restart may come while a user manager runs, as /proc or the
// cgroup tree shows one without the bus, and the words say systemd may be
// about to start the daemon again; with none, nothing can.
func TestARestartMayComeWhileAUserManagerRuns(t *testing.T) {
	noManager(t)
	if why := restartMayCome(); why != "" {
		t.Fatalf("with no user manager: %q, want none", why)
	}
	uid := os.Getuid()
	if err := os.MkdirAll(filepath.Join(cgroupDir, fmt.Sprintf("user.slice/user-%d.slice/user@%d.service", uid, uid)), 0o755); err != nil {
		t.Fatal(err)
	}
	if why := restartMayCome(); why != restartUnknown {
		t.Fatalf("with a user manager: %q, want %q", why, restartUnknown)
	}
}
