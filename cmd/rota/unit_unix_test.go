//go:build linux || darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// Every test here writes units into a temporary HOME and XDG_CONFIG_HOME
// and records the supervisor instead of running it.

// supervisorCalls is what this platform's supervisor is asked, in order, to
// install a store's unit the first time, to install it again, and to
// uninstall it, and where the unit's file is.
type supervisorCalls struct {
	path                          string
	install, reinstall, uninstall []string
}

func expectedCalls(t *testing.T, store string) supervisorCalls {
	t.Helper()
	unit, label := unitName(store)
	switch runtime.GOOS {
	case "linux":
		dir := filepath.Join(os.Getenv("HOME"), ".config")
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			dir = x
		}
		return supervisorCalls{
			path:      filepath.Join(dir, "systemd", "user", unit),
			install:   []string{"systemctl --user daemon-reload", "systemctl --user enable --now " + unit},
			reinstall: []string{"systemctl --user daemon-reload", "systemctl --user enable " + unit, "systemctl --user restart " + unit},
			uninstall: []string{"systemctl --user disable --now " + unit, "systemctl --user daemon-reload"},
		}
	case "darwin":
		path := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", label+".plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		load := []string{"launchctl bootout " + domain + "/" + label, "launchctl bootstrap " + domain + " " + path}
		return supervisorCalls{
			path:      path,
			install:   load,
			reinstall: load,
			uninstall: []string{"launchctl bootout " + domain + "/" + label},
		}
	}
	t.Fatalf("no supervisor on %s", runtime.GOOS)
	return supervisorCalls{}
}

// unitHome points HOME and XDG_CONFIG_HOME (or, with xdg false, nothing)
// at a temporary directory, and returns it.
func unitHome(t *testing.T, xdg bool) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if xdg {
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	} else {
		t.Setenv("XDG_CONFIG_HOME", "")
	}
	return home
}

func TestWriteAndRemoveUnitRunTheSupervisor(t *testing.T) {
	for _, xdg := range []bool{true, false} {
		t.Run(fmt.Sprintf("xdg=%v", xdg), func(t *testing.T) {
			home := unitHome(t, xdg)
			calls := recordUnits(t, nil)
			store := filepath.Join(home, ".rota")
			want := expectedCalls(t, store)
			path, err := writeUnit("/u/rota", store, []string{"PATH=/p"}, false)
			if err != nil {
				t.Fatal(err)
			}
			if path != want.path {
				t.Fatalf("unit at %s, want %s", path, want.path)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(*calls, want.install) {
				t.Fatalf("install asked %q, want %q", *calls, want.install)
			}
			*calls = nil
			if _, err := writeUnit("/u/rota", store, []string{"PATH=/p"}, true); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(*calls, want.reinstall) {
				t.Fatalf("reinstall asked %q, want %q", *calls, want.reinstall)
			}
			*calls = nil
			if err := removeUnit(store); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("unit still there: %v", err)
			}
			if !slices.Equal(*calls, want.uninstall) {
				t.Fatalf("uninstall asked %q, want %q", *calls, want.uninstall)
			}
		})
	}
}

// A unit the supervisor will not take away — a launchd agent that was
// never loaded, say, because its install failed — still has its file
// deleted, and what the supervisor said is the error.
func TestRemovingAUnitDeletesItWhateverTheSupervisorSays(t *testing.T) {
	home := unitHome(t, true)
	store := filepath.Join(home, ".rota")
	recordUnits(t, nil)
	path, err := writeUnit("/u/rota", store, []string{"PATH=/p"}, false)
	if err != nil {
		t.Fatal(err)
	}
	calls := recordUnits(t, func(string) error { return errors.New("No such process") })
	err = removeUnit(store)
	if err == nil || !strings.Contains(err.Error(), "No such process") {
		t.Fatalf("removeUnit: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
	if want := expectedCalls(t, store).uninstall; !slices.Equal(*calls, want) {
		t.Fatalf("asked %q, want %q", *calls, want)
	}
}

// When a supervisor command fails, what it said is in the error, not only
// its exit status.
func TestASupervisorsComplaintIsKept(t *testing.T) {
	dir := t.TempDir()
	if err := runSupervisor("ls", dir); err != nil {
		t.Fatalf("a command that succeeds: %v", err)
	}
	absent := filepath.Join(dir, "absent")
	err := runSupervisor("ls", absent)
	if err == nil || !strings.HasPrefix(err.Error(), "ls: exit status ") || !strings.Contains(err.Error(), absent) {
		t.Fatalf("a command that fails: %v", err)
	}
}

// unitRig is a store whose daemon could be installed: a short ROTA_HOME,
// with an account and a CLAUDE_CONFIG_DIR, in a temporary HOME and
// XDG_CONFIG_HOME, with the supervisor recorded and ROTA_TOKEN set to a
// secret no unit may hold.
type unitRig struct {
	store, terminals string
	calls            *[]string
	want             supervisorCalls
}

const rigSecret = "rig-secret-0123456789"

func newUnitRig(t *testing.T) *unitRig {
	t.Helper()
	shortHome(t)
	unitHome(t, true)
	t.Setenv("ROTA_TOKEN", rigSecret)
	store := os.Getenv("ROTA_HOME")
	return &unitRig{
		store:     store,
		terminals: filepath.Join(store, "terminals"),
		calls:     recordUnits(t, nil),
		want:      expectedCalls(t, store),
	}
}

// unitText is the unit or plist install writes for exe, by this
// platform's renderer, from the rig's environment.
func (r *unitRig) unitText(t *testing.T, exe string) string {
	t.Helper()
	return r.textWith(exe, []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CONFIG_DIR=" + os.Getenv("CLAUDE_CONFIG_DIR")})
}

// textWith is the unit or plist for exe and env, by this platform's
// renderer.
func (r *unitRig) textWith(exe string, env []string) string {
	if runtime.GOOS == "darwin" {
		_, label := unitName(r.store)
		return launchdPlist(label, exe, r.store, env)
	}
	return systemdUnit(exe, r.store, env)
}

func (r *unitRig) checkUnit(t *testing.T, exe string) {
	t.Helper()
	raw, err := os.ReadFile(r.want.path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(raw), r.unitText(t, exe); got != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(string(raw), rigSecret) {
		t.Fatal("ROTA_TOKEN reached the unit")
	}
}

// rota serve install writes this store's unit, running the rota that was
// typed with this PATH and CLAUDE_CONFIG_DIR, has the supervisor load it,
// and says where it is; run again, it rewrites it and restarts it.
func TestInstallRunsTheSupervisor(t *testing.T) {
	r := newUnitRig(t)
	out, errb, code := call(t, "serve", "install")
	if code != 0 || out != fmt.Sprintf("rota daemon installed for %s: %s\n", r.store, r.want.path) {
		t.Fatalf("install: exit %d %q %q", code, out, errb)
	}
	exe, err := invokedAs()
	if err != nil {
		t.Fatal(err)
	}
	r.checkUnit(t, exe)
	if !slices.Equal(*r.calls, r.want.install) {
		t.Fatalf("install asked %q, want %q", *r.calls, r.want.install)
	}
	*r.calls = nil
	t.Setenv("PATH", "/new/bin:"+os.Getenv("PATH"))
	out, errb, code = call(t, "serve", "install")
	if code != 0 || out != fmt.Sprintf("rota daemon reinstalled for %s and restarted: %s\n", r.store, r.want.path) {
		t.Fatalf("install again: exit %d %q %q", code, out, errb)
	}
	r.checkUnit(t, exe)
	if !slices.Equal(*r.calls, r.want.reinstall) {
		t.Fatalf("install again asked %q, want %q", *r.calls, r.want.reinstall)
	}
}

// A supervisor that will not load the unit is said, with what it said, and
// the file it was given is named, with how to take it away.
func TestAnInstallTheSupervisorRefusesIsSaid(t *testing.T) {
	r := newUnitRig(t)
	recordUnits(t, func(string) error { return errors.New("Failed to connect to bus") })
	_, errb, code := call(t, "serve", "install")
	want := fmt.Sprintf("%s is written, but Failed to connect to bus; rota serve uninstall takes it away", r.want.path)
	if code != 1 || !strings.Contains(errb, want) {
		t.Fatalf("install: exit %d %q, want %q", code, errb, want)
	}
}

// rota serve uninstall has the supervisor stop the unit, deletes its file,
// and says how many sessions keep running; with nothing installed it says
// so and asks the supervisor nothing.
func TestUninstallRemovesIt(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	*r.calls = nil
	out, errb, code := call(t, "serve", "uninstall")
	if code != 0 || out != fmt.Sprintf("rota daemon uninstalled for %s; 0 sessions keep running\n", r.store) {
		t.Fatalf("uninstall: exit %d %q %q", code, out, errb)
	}
	if !slices.Equal(*r.calls, r.want.uninstall) {
		t.Fatalf("uninstall asked %q, want %q", *r.calls, r.want.uninstall)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
	*r.calls = nil
	out, errb, code = call(t, "serve", "uninstall")
	if code != 0 || out != fmt.Sprintf("no rota daemon is installed for %s\n", r.store) || len(*r.calls) != 0 {
		t.Fatalf("uninstall again: exit %d %q %q, asked %q", code, out, errb, *r.calls)
	}
}

// Uninstall deletes the file even when the supervisor will not stop the
// unit, and says both.
func TestUninstallSaysWhatTheSupervisorSaid(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	first := r.want.uninstall[0]
	recordUnits(t, func(call string) error {
		if call == first {
			return errors.New("No such process")
		}
		return nil
	})
	_, errb, code := call(t, "serve", "uninstall")
	if code != 1 || !strings.Contains(errb, r.want.path+" is removed, but No such process") {
		t.Fatalf("uninstall: exit %d %q", code, errb)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
}

// A daemon that runs with no unit of this store's installed was started by
// hand: install refuses, writes nothing and asks the supervisor nothing.
func TestInstallRefusesWhileAHandStartedDaemonRuns(t *testing.T) {
	r := newUnitRig(t)
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	out, errb, code := call(t, "serve", "install")
	want := fmt.Sprintf("a rota daemon is running for %s (pid %d), started by hand; rota serve stop it, then install", r.store, os.Getpid())
	if code != 1 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("install: exit %d %q %q, want %q", code, out, errb, want)
	}
	if len(*r.calls) != 0 {
		t.Fatalf("the supervisor was asked %q", *r.calls)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("a unit was written: %v", err)
	}
}

// With the unit installed, a daemon running — the unit's, or one started
// by hand meanwhile — is stopped first, as serve stop stops one: SIGTERM,
// and a wait for the store to be let go of. Only then is the supervisor
// asked to restart the unit, whose daemon so never finds the store taken.
// The claim here is the test's own, and so is the SIGTERM, which it takes
// as the daemon would: by letting go.
func TestReinstallStopsTheDaemonFirst(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	defer signal.Stop(terms)
	termed, done := make(chan struct{}), make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-terms:
			close(termed)
			release()
		case <-done:
		}
	}()
	asked, heldWhenAsked := 0, false
	calls := recordUnits(t, func(string) error {
		if asked++; asked == 1 {
			heldWhenAsked = holdwire.Locked(holdwire.DaemonLock(r.terminals))
		}
		return nil
	})
	out, errb, code := call(t, "serve", "install")
	if code != 0 || out != fmt.Sprintf("rota daemon reinstalled for %s and restarted: %s\n", r.store, r.want.path) {
		t.Fatalf("install over a running daemon: exit %d %q %q", code, out, errb)
	}
	select {
	case <-termed:
	default:
		t.Fatal("the daemon running was not sent SIGTERM")
	}
	if heldWhenAsked {
		t.Fatal("the supervisor was asked while the daemon still held the store")
	}
	if !slices.Equal(*calls, r.want.reinstall) {
		t.Fatalf("asked %q, want %q", *calls, r.want.reinstall)
	}
}

// rota serve uninstall, like serve stop, is refused inside a held session:
// it would end the daemon, and take away what starts it again, from inside
// one by mistake. Once the session is over, the same process may. The
// probe is this binary as a rota in a session of its own, which a held
// session's record names as its holder: sessionProbe, which serve stop's
// test drives too.
func TestUninstallIsRefusedInsideAHeldSession(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	record := filepath.Join(t.TempDir(), "calls")
	probe, ask := sessionProbe(t, append(os.Environ(), "ROTA_UNIT_RECORD="+record))

	const id = "00000000000000e1"
	dir := holdwire.Dir(r.terminals, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	m := &holdwire.Meta{ID: id, Kind: holdwire.KindAccount, Account: 1, Provider: "claude",
		HolderPID: probe, Protocol: holdwire.Major, Version: wire.Version, Started: time.Now()}
	if err := holdwire.WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)

	code, said := ask("serve uninstall")
	if want := "rota serve uninstall is refused inside held session " + id + "; run it from outside the session"; code != 1 || !strings.Contains(said, want) {
		t.Fatalf("uninstall inside a held session: exit %d %q, want %q", code, said, want)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the supervisor was asked from inside a held session: %v", err)
	}
	if _, err := os.Lstat(r.want.path); err != nil {
		t.Fatalf("the unit was taken away from inside a held session: %v", err)
	}

	release()
	if code, said := ask("serve uninstall"); code != 0 || said != fmt.Sprintf("rota daemon uninstalled for %s; 0 sessions keep running\n", r.store) {
		t.Fatalf("uninstall once the session was over: exit %d %q", code, said)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); !slices.Equal(got, r.want.uninstall) {
		t.Fatalf("asked %q, want %q", got, r.want.uninstall)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
}

// The unit runs the rota that was typed, by the path it was typed as:
// a link an upgrade moves on, such as Homebrew's, is kept as a link, so
// the daemon a restart starts is the upgraded one. The test binary is run
// through a link, as rota, and records its supervisor to a file.
func TestTheUnitRunsTheRotaThatWasTyped(t *testing.T) {
	r := newUnitRig(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "rota")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "calls")
	cmd := exec.Command(link, "serve", "install")
	cmd.Env = append(os.Environ(), "ROTA_AS_RUN=1", "ROTA_UNIT_RECORD="+record)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install: %v: %s", err, out)
	}
	r.checkUnit(t, link)
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); !slices.Equal(got, r.want.install) {
		t.Fatalf("asked %q, want %q", got, r.want.install)
	}
}

// Inside a CLI rota launched, CLAUDE_CONFIG_DIR is the account's mirror and
// ROTA_CLAUDE_HOME the person's own directory, which is what the unit
// carries, by absolute path; set but empty, the person's is the default,
// and the unit names none.
func TestTheUnitCarriesThePersonsClaudeDirectory(t *testing.T) {
	r := newUnitRig(t)
	exe, err := invokedAs()
	if err != nil {
		t.Fatal(err)
	}
	mirror, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", mirror)
	t.Chdir(cwd)
	for _, c := range []struct{ home, want string }{
		{filepath.Join(cwd, "real"), filepath.Join(cwd, "real")},
		{"real", filepath.Join(cwd, "real")},
		{"", ""},
	} {
		t.Setenv("ROTA_CLAUDE_HOME", c.home)
		if _, errb, code := call(t, "serve", "install"); code != 0 {
			t.Fatalf("install with ROTA_CLAUDE_HOME=%q: exit %d %q", c.home, code, errb)
		}
		raw, err := os.ReadFile(r.want.path)
		if err != nil {
			t.Fatal(err)
		}
		env := []string{"PATH=" + os.Getenv("PATH")}
		if c.want != "" {
			env = append(env, "CLAUDE_CONFIG_DIR="+c.want)
		}
		if got, want := string(raw), r.textWith(exe, env); got != want {
			t.Fatalf("ROTA_CLAUDE_HOME=%q: unit:\n%s\nwant:\n%s", c.home, got, want)
		}
		if strings.Contains(string(raw), mirror) {
			t.Fatalf("ROTA_CLAUDE_HOME=%q: the account's mirror reached the unit", c.home)
		}
	}
}

// A file the daemon would refuse is refused by install, before anything
// is written: a unit that cannot start would fail again every five seconds.
func TestInstallRefusesWhatTheDaemonWould(t *testing.T) {
	r := newUnitRig(t)
	conf := filepath.Join(r.store, "server.toml")
	if err := os.WriteFile(conf, []byte("[runs]\nroots = [\"work\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, errb, code := call(t, "serve", "install")
	if code != 1 || !strings.Contains(errb, `runs.roots: "work" is relative`) {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	if len(*r.calls) != 0 {
		t.Fatalf("the supervisor was asked %q", *r.calls)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("a unit was written: %v", err)
	}
}
