//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
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

// No test asks systemctl to reach this machine's own user manager: where
// its runtime directory is looked for is somewhere that is never there,
// unless a test points it elsewhere.
func init() { runtimeBase = "/nonexistent/rota-test/run/user" }

// noManager points where the user manager is looked for at a /proc that
// shows only itself and an empty cgroup tree, until the test ends: none
// runs.
func noManager(t *testing.T) {
	t.Helper()
	oldProc, oldCgroup := procDir, cgroupDir
	procDir, cgroupDir = t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(procDir, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { procDir, cgroupDir = oldProc, oldCgroup })
}

// systemctl --user from sudo -u, su or cron has no XDG_RUNTIME_DIR that
// reaches a manager — none, the invoker's, or one of this user's own with
// no manager in it. An XDG_RUNTIME_DIR with a manager's socket in it is
// left alone, whoever's it is: root given a user's, as sudo -E gives it,
// reaches that user's manager, which has the unit. Otherwise, when this
// user's manager listens at /run/user/<uid>/systemd/private, its own socket
// in its own directory, systemctl is given that directory: the same
// manager, user@<uid>.service, without a bus.
func TestSystemctlIsGivenTheUserManagersRuntimeDirectory(t *testing.T) {
	uid := os.Getuid()
	root := t.TempDir()
	base := filepath.Join(root, "u")
	dir := filepath.Join(base, fmt.Sprint(uid))
	other := filepath.Join(root, "o")
	for _, d := range []string{filepath.Join(dir, "systemd"), filepath.Join(other, "systemd")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := runtimeBase
	runtimeBase = base
	t.Cleanup(func() { runtimeBase = old })
	private := filepath.Join(dir, "systemd", "private")
	ask := func(t *testing.T, xdg string) []string {
		t.Helper()
		t.Setenv("XDG_RUNTIME_DIR", xdg)
		calls := recordUnits(t, nil)
		if err := systemctl("daemon-reload"); err != nil {
			t.Fatal(err)
		}
		return *calls
	}
	plain := []string{"systemctl --user daemon-reload"}
	given := []string{"XDG_RUNTIME_DIR=" + dir + " systemctl --user daemon-reload"}
	if got := ask(t, ""); !slices.Equal(got, plain) {
		t.Fatalf("no socket: asked %q, want %q", got, plain)
	}
	if err := os.WriteFile(private, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ask(t, ""); !slices.Equal(got, plain) {
		t.Fatalf("a file, not a socket: asked %q, want %q", got, plain)
	}
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{private, filepath.Join(other, "systemd", "private")} {
		l, err := net.Listen("unix", p)
		if err != nil {
			t.Skipf("no socket here: %v", err)
		}
		t.Cleanup(func() { l.Close() })
	}
	for _, c := range []struct {
		name, xdg string
		want      []string
	}{
		{"unset", "", given},
		{"one that is not there", filepath.Join(root, "absent"), given},
		{"this user's own, with no manager in it", t.TempDir(), given},
		{"one with a manager in it", other, plain},
	} {
		if got := ask(t, c.xdg); !slices.Equal(got, c.want) {
			t.Fatalf("XDG_RUNTIME_DIR %s: asked %q, want %q", c.name, got, c.want)
		}
	}
	// Whose each is, as a seam says, so that a directory or socket not this
	// user's can be had without root, and as root.
	real := ownerOf
	t.Cleanup(func() { ownerOf = real })
	for _, c := range []struct {
		name, foreign, xdg string
		want               []string
	}{
		{"somebody else's, with a manager in it", "o", other, plain},
		{"/run/user/<uid> somebody else's", fmt.Sprint(uid), "", plain},
		{"its socket somebody else's", "private", "", plain},
	} {
		ownerOf = func(fi fs.FileInfo) int {
			if fi.Name() == c.foreign {
				return 65534
			}
			return real(fi)
		}
		if got := ask(t, c.xdg); !slices.Equal(got, c.want) {
			t.Fatalf("%s: asked %q, want %q", c.name, got, c.want)
		}
	}
}

// ownedBy compares the owner's uid with the one asked for, and the kind:
// a directory of this user's is this user's, not another uid's, and not a
// socket. Nothing here needs another user, so root is checked as well.
func TestOwnedByComparesTheUid(t *testing.T) {
	uid, dir := os.Getuid(), t.TempDir()
	if !ownedBy(dir, uid, fs.ModeDir) {
		t.Fatalf("%s is not taken for uid %d's", dir, uid)
	}
	for _, c := range []struct {
		uid  int
		kind fs.FileMode
	}{{uid + 1, fs.ModeDir}, {65534, fs.ModeDir}, {uid, fs.ModeSocket}} {
		if ownedBy(dir, c.uid, c.kind) {
			t.Fatalf("%s is taken for uid %d's %v", dir, c.uid, c.kind)
		}
	}
	if ownedBy(filepath.Join(dir, "absent"), uid, fs.ModeDir) {
		t.Fatal("a path that is not there is taken for one")
	}
}

// The daemon running is the unit's when its cgroup, on either layout, is
// the unit's; one anywhere else, or whose cgroup cannot be read, is not
// taken for it.
func TestADaemonIsTheUnitsByItsCgroup(t *testing.T) {
	store := filepath.Join(unitHome(t, true), ".rota")
	name, _ := unitName(store)
	old := procDir
	procDir = t.TempDir()
	t.Cleanup(func() { procDir = old })
	for pid, c := range []struct {
		cgroup string
		under  bool
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + name + "\n", true},
		{"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/" + name + "\n0::/\n", true},
		{"0::/user.slice/user-1000.slice/session-3.scope\n", false},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + name + "x\n", false},
		{"", false},
	} {
		pid++
		dir := filepath.Join(procDir, fmt.Sprint(pid))
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if c.cgroup != "" {
			if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(c.cgroup), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := underUnit(store, pid); got != c.under {
			t.Errorf("cgroup %q: under the unit %v, want %v", c.cgroup, got, c.under)
		}
	}
}

// The way back is said as what to do: from a login of the person's own when
// systemctl could not reach their manager — naming the socket sudo -u, su
// and cron reach it by — and otherwise once systemctl can do it.
func TestTheWayBackIsSaid(t *testing.T) {
	old := runtimeBase
	runtimeBase = "/run/user"
	t.Cleanup(func() { runtimeBase = old })
	for _, c := range []struct {
		said, want string
	}{
		{"systemctl --user disable --now rota.service: exit status 1: Failed to connect to user scope bus via local transport: Connection refused",
			fmt.Sprintf("run rota serve uninstall again from an ssh or console login of yours, or from sudo -u, su or cron while your user manager listens at /run/user/%d/systemd/private", os.Getuid())},
		{"systemctl --user disable --now rota.service: exit status 1: Failed to connect to bus: No medium found",
			fmt.Sprintf("run rota serve uninstall again from an ssh or console login of yours, or from sudo -u, su or cron while your user manager listens at /run/user/%d/systemd/private", os.Getuid())},
		{"systemctl --user disable --now rota.service: exit status 1: Failed to disable unit: Read-only file system",
			"run rota serve uninstall again once systemctl --user can stop the unit"},
	} {
		if got := againFrom(errors.New(c.said)); got != c.want {
			t.Errorf("after %q:\n%q\nwant %q", c.said, got, c.want)
		}
	}
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
