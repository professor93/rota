//go:build linux || darwin

package main

import (
	"encoding/json"
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

	"github.com/professor93/rota/internal/fakecli"
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
			if _, err := removeUnit(store, filepath.Join(store, "terminals")); err != nil {
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

// restartMayComeAs has restartMayCome say why, until the test ends: why the
// supervisor may yet start the unit's daemon again, or "" for none will.
func restartMayComeAs(t *testing.T, why string) {
	t.Helper()
	old := restartMayCome
	restartMayCome = func() string { return why }
	t.Cleanup(func() { restartMayCome = old })
}

// A unit the supervisor will not take away — with no bus to reach it, say
// — and with no daemon running has its file deleted only when no
// supervisor can start the daemon again; it asks for no reload, which
// nobody would hear. When one may, the file and its link are kept, and the
// error says why. Either way what the supervisor said is the error.
func TestRemovingAUnitTheSupervisorRefuses(t *testing.T) {
	for _, why := range []string{"", "the supervisor may start it again"} {
		t.Run(fmt.Sprintf("restart=%q", why), func(t *testing.T) {
			home := unitHome(t, true)
			store := filepath.Join(home, ".rota")
			recordUnits(t, nil)
			path, err := writeUnit("/u/rota", store, []string{"PATH=/p"}, false)
			if err != nil {
				t.Fatal(err)
			}
			link := wantsLink(t, path)
			restartMayComeAs(t, why)
			calls := recordUnits(t, func(string) error { return errors.New("No such process") })
			fate, err := removeUnit(store, filepath.Join(store, "terminals"))
			if fate != nil || err == nil || !strings.Contains(err.Error(), "No such process") {
				t.Fatalf("removeUnit: %+v %v", fate, err)
			}
			var kept unitKept
			if errors.As(err, &kept) != (why != "") || kept.restart != why || kept.daemon != nil {
				t.Fatalf("removeUnit: %#v, want kept for %q", err, why)
			}
			for _, p := range []string{path, link} {
				if _, err := os.Lstat(p); p != "" && os.IsNotExist(err) != (why == "") {
					t.Fatalf("%s: %v, want it gone only when no restart may come", p, err)
				}
			}
			if want := expectedCalls(t, store).uninstall[:1]; !slices.Equal(*calls, want) {
				t.Fatalf("asked %q, want %q", *calls, want)
			}
		})
	}
}

// With no daemon running, an uninstall the supervisor refuses keeps the
// unit while the supervisor may be about to start the daemon again — after
// a crash, systemd waits 5 seconds — since a daemon it starts from a unit
// whose file is gone would take the unit's sessions with it as it stops.
// It says so in one line, and never that the unit is removed. So does one
// whose daemon went on its own between the look and the signal: whether it
// crashed cannot be told.
func TestAnUninstallKeepsTheUnitWhileARestartMayCome(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprintf("went=%v", gone), func(t *testing.T) {
			r := newUnitRig(t)
			if _, errb, code := call(t, "serve", "install"); code != 0 {
				t.Fatalf("install: exit %d %q", code, errb)
			}
			link := wantsLink(t, r.want.path)
			unreachableSupervisor(t)
			const why = "the supervisor may be about to start one again"
			restartMayComeAs(t, why)
			who := "no rota daemon is running"
			if gone {
				release, err := holdwire.ClaimDaemon(r.terminals, "test")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(release)
				old := signalTheDaemon
				signalTheDaemon = func(terminals string) (*holdwire.Daemon, error) {
					release()
					return signalDaemon(terminals)
				}
				t.Cleanup(func() { signalTheDaemon = old })
				playExit(t, func(time.Duration) bool { return true })
				who = fmt.Sprintf("the rota daemon (pid %d) is no longer running", os.Getpid())
			}
			out, errb, code := call(t, "serve", "uninstall")
			want := fmt.Sprintf("error: %s: exit status 1: Failed to connect to user scope bus via local transport: "+
				"$DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined; %s, but %s, so %s is kept: %s; %s\n",
				r.want.uninstall[0], who, why, r.want.path, keptWhy, againFrom(errors.New(noBus)))
			if code != 1 || out != "" || errb != want {
				t.Fatalf("uninstall while a restart may come: exit %d %q\n%q\nwant %q", code, out, errb, want)
			}
			for _, p := range []string{r.want.path, link} {
				if _, err := os.Lstat(p); p != "" && err != nil {
					t.Fatalf("%s was taken away: %v", p, err)
				}
			}
		})
	}
}

// When a supervisor command fails, the error names it and says what it
// said, not only its exit status, each with a space for an escape in it.
func TestASupervisorsComplaintIsKept(t *testing.T) {
	dir := t.TempDir()
	if err := runSupervisor("ls", dir); err != nil {
		t.Fatalf("a command that succeeds: %v", err)
	}
	absent := filepath.Join(dir, "absent")
	err := runSupervisor("ls", absent)
	if err == nil || !strings.HasPrefix(err.Error(), "ls "+absent+": exit status ") || !strings.Contains(err.Error(), "No such file or directory") {
		t.Fatalf("a command that fails: %v", err)
	}
	err = runSupervisor("ls", absent+"\x1b[2J")
	if err == nil || !strings.HasPrefix(err.Error(), "ls "+absent+" [2J: exit status ") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("a command named with an escape: %q", err)
	}
}

// A supervisor command may start with NAME=VALUE words, as env(1) takes
// them: each is set in its environment, over what it inherits, and the
// command is named with them when it fails.
func TestASupervisorCommandTakesTheEnvironmentItIsGiven(t *testing.T) {
	probe := fakecli.Install(t, t.TempDir(), "probe", fakecli.Spec{Stderr: []string{"runtime {{env:XDG_RUNTIME_DIR|unset}}"}, Exit: 1})
	t.Setenv("XDG_RUNTIME_DIR", "/inherited")
	for _, c := range []struct {
		argv  []string
		named string
		saw   string
	}{
		{[]string{probe, "x"}, probe + " x", "runtime /inherited"},
		{[]string{"XDG_RUNTIME_DIR=/run/user/7", probe, "x"}, "XDG_RUNTIME_DIR=/run/user/7 " + probe + " x", "runtime /run/user/7"},
	} {
		err := runSupervisor(c.argv[0], c.argv[1:]...)
		if err == nil || !strings.HasPrefix(err.Error(), c.named+": exit status 1: ") || !strings.HasSuffix(err.Error(), c.saw) {
			t.Fatalf("%q: %v, want it named %q and %q said", c.argv, err, c.named, c.saw)
		}
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

// A store whose path has an escape and a bell in it is said with a space
// for each in every line rota serve install, status, stop and uninstall
// print, and status --json carries the path as it is. The name is short,
// so a held session's socket still fits under it, which install asks.
func TestServeSaysADirtyStorePathCleaned(t *testing.T) {
	shortHome(t)
	store := filepath.Join(os.Getenv("ROTA_HOME"), "s\x1b\x07")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := holdwire.SocketsFit(filepath.Join(store, "terminals")); err != nil {
		t.Skipf("no held session fits here: %v", err)
	}
	t.Setenv("ROTA_HOME", store)
	unitHome(t, true)
	recordUnits(t, nil)
	unit := expectedCalls(t, store).path
	drawn := strings.TrimSuffix(store, "s\x1b\x07") + "s  "
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"serve", "install"}, 0, "rota daemon installed for " + drawn + ": " + unit + "\n"},
		{[]string{"serve", "status"}, 3, "no rota daemon is running for " + drawn + "\n"},
		{[]string{"serve", "stop"}, 0, "no rota daemon is running for " + drawn + "\n"},
		{[]string{"serve", "stop", "--all", "--yes"}, 0, "no rota daemon is running for " + drawn + ", and no session is\n"},
		{[]string{"serve", "uninstall"}, 0, "rota daemon uninstalled for " + drawn + "; 0 sessions keep running\n"},
	} {
		out, errb, code := call(t, c.args...)
		if code != c.code || out != c.want || strings.ContainsAny(out+errb, "\x1b\x07") {
			t.Errorf("%s: exit %d %q %q; want exit %d %q", strings.Join(c.args, " "), code, out, errb, c.code, c.want)
		}
	}
	out, errb, code := call(t, "--json", "serve", "status")
	var doc statusJSON
	if code != 3 || json.Unmarshal([]byte(out), &doc) != nil || doc.Store != store || doc.Ctl != holdwire.CtlSock(filepath.Join(store, "terminals")) {
		t.Fatalf("status --json: exit %d %q %q; want the store as it is, %q", code, out, errb, store)
	}
}

// Uninstall deletes the file even when the supervisor will not stop the
// unit, as long as no supervisor can start its daemon again, and says both.
// With no daemon running it says nothing of one.
func TestUninstallSaysWhatTheSupervisorSaid(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	restartMayComeAs(t, "")
	first := r.want.uninstall[0]
	recordUnits(t, func(call string) error {
		if call == first {
			return errors.New("No such process")
		}
		return nil
	})
	_, errb, code := call(t, "serve", "uninstall")
	if code != 1 || !strings.Contains(errb, r.want.path+" is removed, but No such process") || strings.Contains(errb, "rota daemon") {
		t.Fatalf("uninstall: exit %d %q, want the supervisor's words and nothing of a daemon", code, errb)
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
	termed := daemonByTheTest(t, r.terminals)
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

// daemonByTheTest has the test hold the store's daemon claim, as a daemon
// would, and take the SIGTERM a stop sends as a daemon does: by letting go.
// The channel is closed once it has been sent one.
func daemonByTheTest(t *testing.T, terminals string) <-chan struct{} {
	t.Helper()
	return daemonByTheTestThen(t, terminals, func() bool { return true })
}

// daemonByTheTestThen is daemonByTheTest with onTerm run as the SIGTERM
// arrives: the claim is let go of only when it says so, and otherwise held
// until the test ends, as by a daemon that will not stop.
func daemonByTheTestThen(t *testing.T, terminals string, onTerm func() (letGo bool)) <-chan struct{} {
	t.Helper()
	release, err := holdwire.ClaimDaemon(terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	terms := make(chan os.Signal, 1)
	signal.Notify(terms, syscall.SIGTERM)
	termed, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(done); signal.Stop(terms); release() })
	go func() {
		select {
		case <-terms:
			letGo := onTerm()
			close(termed)
			if letGo {
				release()
			}
		case <-done:
		}
	}()
	return termed
}

// noBus is what systemctl --user says with no user bus to reach.
const noBus = "Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined"

// daemonUnderUnitAs has daemonUnderUnit say under, until the test ends:
// whether the daemon running is the one the unit started.
func daemonUnderUnitAs(t *testing.T, under bool) {
	t.Helper()
	old := daemonUnderUnit
	daemonUnderUnit = func(string, int) bool { return under }
	t.Cleanup(func() { daemonUnderUnit = old })
}

// unreachableSupervisor puts a supervisor on PATH that fails every command
// as systemctl --user does with no user bus to reach, and has rota run the
// real one it finds there: a fake built from this test binary, not a script.
func unreachableSupervisor(t *testing.T) {
	t.Helper()
	name := map[string]string{"linux": "systemctl", "darwin": "launchctl"}[runtime.GOOS]
	bin := t.TempDir()
	fakecli.Install(t, bin, name, fakecli.Spec{
		Stderr: []string{noBus},
		Exit:   1,
	})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := unitRun
	unitRun = runSupervisor
	t.Cleanup(func() { unitRun = old })
}

// wantsLink makes the link systemctl enable makes for the unit at path, and
// returns it; launchd has none, and there it is "".
func wantsLink(t *testing.T, path string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		return ""
	}
	link := filepath.Join(filepath.Dir(path), "default.target.wants", filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// An uninstall the supervisor cannot be asked for — no user bus to reach,
// say — may leave the daemon running under the unit, and systemd, reloading
// a unit whose file is gone, would end every session in it as the daemon
// stops. So the daemon is stopped first, as serve stop stops one, while the
// file is still there; only then are the file and its link taken away, and
// no reload is asked for, which could catch the daemon on its way out. What
// it says names the supervisor's complaint and the daemon it stopped.
func TestAnUninstallTheSupervisorRefusesStopsTheDaemonFirst(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	link := wantsLink(t, r.want.path)
	unreachableSupervisor(t)
	// The unit's own daemon: while it runs, no restart of the unit can be
	// pending, so whether one may come is not asked.
	daemonUnderUnitAs(t, true)
	restartMayComeAs(t, "never asked")
	fileAtTerm := make(chan bool, 1)
	termed := daemonByTheTestThen(t, r.terminals, func() bool {
		_, err := os.Lstat(r.want.path)
		fileAtTerm <- err == nil
		return true
	})
	// The daemon is this process, which goes on after it lets go of the
	// store, as a daemon still serving does: it is taken to exit only at
	// the third look, and the file must be there at each look before.
	var looks []bool
	playExit(t, func(within time.Duration) bool {
		_, err := os.Lstat(r.want.path)
		looks = append(looks, err == nil)
		return len(looks) == 3
	})
	out, errb, code := call(t, "serve", "uninstall")
	for _, want := range []string{
		r.want.path + " is removed, but " + r.want.uninstall[0] + ": exit status 1: Failed to connect to user scope bus",
		fmt.Sprintf("; the rota daemon (pid %d) was stopped as rota serve stop stops one, and 0 sessions keep running", os.Getpid()),
	} {
		if code != 1 || out != "" || !strings.Contains(errb, want) {
			t.Fatalf("uninstall with no supervisor: exit %d %q %q, want %q", code, out, errb, want)
		}
	}
	if strings.Contains(errb, "daemon-reload") {
		t.Fatalf("a reload was asked for after the daemon was stopped: %q", errb)
	}
	select {
	case <-termed:
	default:
		t.Fatal("the daemon running was not sent SIGTERM")
	}
	if !<-fileAtTerm {
		t.Fatal("the unit's file was gone before the daemon was stopped")
	}
	if !slices.Equal(looks, []bool{true, true, true}) {
		t.Fatalf("the daemon's exit was waited for %d times, the file there at each: %v; want 3, all there", len(looks), looks)
	}
	for _, p := range []string{r.want.path, link} {
		if _, err := os.Lstat(p); p != "" && !os.IsNotExist(err) {
			t.Fatalf("%s still there: %v", p, err)
		}
	}
}

// A daemon running that is not the unit's — one started by hand, as rota
// run --detach starts one, while the unit waits out its restart after its
// own crashed — is stopped as any is, and then the unit is kept while the
// supervisor may start its daemon again: stopping this one cancelled no
// restart. The line says the daemon was stopped, and why the file stays.
func TestAnUninstallKeepsTheUnitAfterStoppingADaemonNotTheUnits(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	link := wantsLink(t, r.want.path)
	unreachableSupervisor(t)
	daemonUnderUnitAs(t, false)
	const why = "the supervisor may be about to start one again"
	restartMayComeAs(t, why)
	termed := daemonByTheTest(t, r.terminals)
	playExit(t, func(time.Duration) bool { return true })
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("error: %s: exit status 1: %s; the rota daemon (pid %d) was stopped as rota serve stop stops one, and 0 sessions keep running, "+
		"but %s, so %s is kept: %s; %s\n",
		r.want.uninstall[0], noBus, os.Getpid(), why, r.want.path, keptWhy, againFrom(errors.New(noBus)))
	if code != 1 || out != "" || errb != want {
		t.Fatalf("uninstall over a daemon not the unit's: exit %d %q\n%q\nwant %q", code, out, errb, want)
	}
	<-termed
	for _, p := range []string{r.want.path, link} {
		if _, err := os.Lstat(p); p != "" && err != nil {
			t.Fatalf("%s was taken away: %v", p, err)
		}
	}
}

// The daemon signalled is not always the one looked at: one may have gone
// and another come between. Whose the one signalled was is then not known
// — the cgroup read was the first one's — so it is taken for one started
// by hand, and the unit is kept while a restart may come, even though the
// first one was the unit's.
func TestAnUninstallKeepsTheUnitWhenTheDaemonChangedMeanwhile(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	unreachableSupervisor(t)
	daemonUnderUnitAs(t, true)
	const why = "the supervisor may be about to start one again"
	restartMayComeAs(t, why)
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	next := &holdwire.Daemon{PID: os.Getpid() + 1}
	old := signalTheDaemon
	signalTheDaemon = func(string) (*holdwire.Daemon, error) {
		release()
		return next, nil
	}
	t.Cleanup(func() { signalTheDaemon = old })
	playExit(t, func(time.Duration) bool { return true })
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("error: %s: exit status 1: %s; the rota daemon (pid %d) was stopped as rota serve stop stops one, and 0 sessions keep running, "+
		"but %s, so %s is kept: %s; %s\n",
		r.want.uninstall[0], noBus, next.PID, why, r.want.path, keptWhy, againFrom(errors.New(noBus)))
	if code != 1 || out != "" || errb != want {
		t.Fatalf("uninstall over a daemon changed meanwhile: exit %d %q\n%q\nwant %q", code, out, errb, want)
	}
	if _, err := os.Lstat(r.want.path); err != nil {
		t.Fatalf("the file was taken away: %v", err)
	}
}

// playExit stands in for the wait for the daemon's process to exit, which
// for a daemon that is this test process would never end: exited is asked
// instead, once for each look, until it says yes.
func playExit(t *testing.T, exited func(within time.Duration) bool) {
	t.Helper()
	old := daemonExited
	daemonExited = func(_ *holdwire.Daemon, within time.Duration) bool {
		for !exited(within) {
		}
		return true
	}
	t.Cleanup(func() { daemonExited = old })
}

// A real daemon, a process of its own, is waited for until it has exited,
// not only until it lets go of the store, before the unit's file goes: in
// between it still serves what is in flight, as the unit's main process,
// and a reload by anybody then would take its sessions with it.
func TestAnUninstallWaitsForTheDaemonsProcessToExit(t *testing.T) {
	r := newDaemonRig(t)
	unitHome(t, true)
	recordUnits(t, nil)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	path := expectedCalls(t, r.home).path
	pid, _ := r.start()
	d, ok := holdwire.RunningDaemon(r.terminals)
	if !ok || d.PID != pid {
		t.Fatalf("daemon.pid says %+v, want pid %d", d, pid)
	}
	unreachableSupervisor(t)
	daemonUnderUnitAs(t, true)
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("; the rota daemon (pid %d) was stopped as rota serve stop stops one, and 0 sessions keep running", pid)
	if code != 1 || out != "" || !strings.Contains(errb, path+" is removed, but ") || !strings.Contains(errb, want) {
		t.Fatalf("uninstall with no supervisor: exit %d %q %q, want %q\nlog:\n%s", code, out, errb, want, r.log())
	}
	// Exited, a zombie included: under a pid 1 that never reaps, as in a
	// bare container, one is left for good.
	exited := make(chan struct{})
	if wait, err := exitOf(pid); err != nil {
		t.Fatal(err)
	} else {
		go func() { wait(); close(exited) }()
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatalf("the daemon (pid %d) was still running when uninstall said it was stopped", pid)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
}

// A daemon that lets go of the store and then does not exit within
// daemonExitWait keeps the unit's file and its link, as one that will not
// stop does, and uninstall says why. The daemon is this process, which
// never exits, and the real wait is asked.
func TestAnUninstallKeepsTheUnitWhileTheDaemonsProcessLingers(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	link := wantsLink(t, r.want.path)
	unreachableSupervisor(t)
	old := daemonExitWait
	daemonExitWait = 300 * time.Millisecond
	t.Cleanup(func() { daemonExitWait = old })
	termed := daemonByTheTest(t, r.terminals)
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("; the rota daemon (pid %d) is still running and could not be stopped (it let go of the store but has not exited within 300ms), so %s is kept: %s; %s",
		os.Getpid(), r.want.path, keptWhy, againFrom(errors.New(noBus)))
	if code != 1 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("uninstall while the daemon lingers: exit %d %q %q, want %q", code, out, errb, want)
	}
	for _, never := range []string{"is removed", "was stopped", "uninstalled"} {
		if strings.Contains(errb, never) {
			t.Fatalf("it says %q: %q", never, errb)
		}
	}
	<-termed
	for _, p := range []string{r.want.path, link} {
		if _, err := os.Lstat(p); p != "" && err != nil {
			t.Fatalf("%s was taken away: %v", p, err)
		}
	}
}

// When the daemon was stopped but the file cannot be deleted, uninstall
// still says the daemon was stopped and its sessions keep running, and that
// the file is kept, after what went wrong.
func TestAnUninstallThatCannotDeleteTheFileSaysTheDaemonStopped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a directory it may not write")
	}
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	unreachableSupervisor(t)
	daemonUnderUnitAs(t, true)
	termed := daemonByTheTest(t, r.terminals)
	playExit(t, func(time.Duration) bool { return true })
	dir := filepath.Dir(r.want.path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	out, errb, code := call(t, "serve", "uninstall")
	for _, want := range []string{
		r.want.uninstall[0] + ": exit status 1: Failed to connect to user scope bus",
		"permission denied",
		fmt.Sprintf("; the rota daemon (pid %d) was stopped as rota serve stop stops one, and 0 sessions keep running; %s is kept", os.Getpid(), r.want.path),
	} {
		if code != 1 || out != "" || !strings.Contains(errb, want) {
			t.Fatalf("uninstall that cannot delete the file: exit %d %q %q, want %q", code, out, errb, want)
		}
	}
	if strings.Contains(errb, "is removed") {
		t.Fatalf("it says the file is removed: %q", errb)
	}
	<-termed
	if _, err := os.Lstat(r.want.path); err != nil {
		t.Fatalf("the file: %v", err)
	}
}

// A daemon that went on its own between uninstall's look and its signal was
// not stopped by uninstall, and is said to be no longer running; the unit's
// file goes, since nothing runs under it and no supervisor can start it
// again.
func TestAnUninstallSaysADaemonThatWentOnItsOwnIsNoLongerRunning(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	unreachableSupervisor(t)
	restartMayComeAs(t, "")
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	old := signalTheDaemon
	signalTheDaemon = func(terminals string) (*holdwire.Daemon, error) {
		release()
		return signalDaemon(terminals)
	}
	t.Cleanup(func() { signalTheDaemon = old })
	playExit(t, func(time.Duration) bool { return true })
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("%s is removed, but %s: exit status 1: Failed to connect to user scope bus via local transport: "+
		"$DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined; the rota daemon (pid %d) is no longer running, and 0 sessions keep running",
		r.want.path, r.want.uninstall[0], os.Getpid())
	if code != 1 || out != "" || strings.TrimSpace(errb) != "error: "+want {
		t.Fatalf("uninstall after the daemon went: exit %d %q\n%q\nwant %q", code, out, errb, want)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("unit still there: %v", err)
	}
}

// A daemon holding the store that daemon.pid names no process for is not
// signalled, and uninstall keeps the unit saying a rota daemon is still
// running, never with a pid the file got wrong as its own: whether the file
// names a process that is not the daemon, as when the daemon is this test
// process and the file a stranger's, or the daemon changes between the look
// and the signal, which only a stand-in can arrange.
func TestAnUninstallKeepsTheUnitForADaemonItCannotTell(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	unreachableSupervisor(t)
	release, err := holdwire.ClaimDaemon(r.terminals, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	pidFile := holdwire.DaemonPIDFile(r.terminals)
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var d holdwire.Daemon
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	d.ProcStart, d.ProcStartTicks = d.ProcStart.Add(-time.Hour), d.ProcStartTicks+1
	if raw, err = json.Marshal(&d); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	self := fmt.Sprintf("(pid %d)", os.Getpid())
	for _, c := range []struct {
		name string
		why  string
		play bool
	}{
		{"a stranger's pid", fmt.Sprintf("a rota daemon holds %s, but %s names no process this rota can check; nothing was signalled",
			holdwire.DaemonLock(r.terminals), pidFile), false},
		{"a daemon changed meanwhile", fmt.Sprintf("pid %d is no longer the rota daemon %s names; nothing was signalled", os.Getpid(), pidFile), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.play {
				old := signalTheDaemon
				signalTheDaemon = func(string) (*holdwire.Daemon, error) { return nil, unnamedDaemon{c.why} }
				t.Cleanup(func() { signalTheDaemon = old })
			}
			out, errb, code := call(t, "serve", "uninstall")
			want := fmt.Sprintf("; a rota daemon is still running and could not be stopped (%s), so %s is kept: ", c.why, r.want.path)
			if code != 1 || out != "" || !strings.Contains(errb, want) || strings.Contains(errb, "the rota daemon "+self) {
				t.Fatalf("uninstall over a daemon it cannot tell: exit %d %q %q, want %q", code, out, errb, want)
			}
			if _, err := os.Lstat(r.want.path); err != nil {
				t.Fatalf("the file was taken away: %v", err)
			}
		})
	}
}

// A daemon that cannot be stopped either keeps the unit's file and its link
// where they are, and uninstall fails saying so: the daemon is running
// still, so the file is kept, and where to run uninstall again from. It
// never says the unit is removed or the daemon stopped.
func TestAnUninstallThatCannotStopTheDaemonKeepsTheUnit(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	link := wantsLink(t, r.want.path)
	unreachableSupervisor(t)
	old := daemonStopWait
	daemonStopWait = 300 * time.Millisecond
	t.Cleanup(func() { daemonStopWait = old })
	termed := daemonByTheTestThen(t, r.terminals, func() bool { return false })
	out, errb, code := call(t, "serve", "uninstall")
	pid := os.Getpid()
	want := fmt.Sprintf("%s: exit status 1: Failed to connect to user scope bus via local transport: "+
		"$DBUS_SESSION_BUS_ADDRESS and $XDG_RUNTIME_DIR not defined; "+
		"the rota daemon (pid %d) is still running and could not be stopped (the rota daemon (pid %d) has not stopped within 300ms), "+
		"so %s is kept: %s; %s",
		r.want.uninstall[0], pid, pid, r.want.path, keptWhy, againFrom(errors.New(noBus)))
	if code != 1 || out != "" || strings.TrimSpace(errb) != "error: "+want {
		t.Fatalf("uninstall that cannot stop the daemon: exit %d %q\n%q\nwant %q", code, out, errb, want)
	}
	for _, never := range []string{"is removed", "was stopped", "uninstalled"} {
		if strings.Contains(errb, never) {
			t.Fatalf("it says %q: %q", never, errb)
		}
	}
	<-termed
	for _, p := range []string{r.want.path, link} {
		if _, err := os.Lstat(p); p != "" && err != nil {
			t.Fatalf("%s was taken away: %v", p, err)
		}
	}
}

// A reinstall that stopped the daemon running and then could not have the
// unit restarted says so: the daemon is stopped, its sessions keep running,
// and serve start brings one back.
func TestAFailedReinstallSaysTheDaemonStopped(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	termed := daemonByTheTest(t, r.terminals)
	recordUnits(t, func(string) error { return errors.New("Failed to connect to bus") })
	_, errb, code := call(t, "serve", "install")
	for _, want := range []string{
		r.want.path + " is written, but Failed to connect to bus; rota serve uninstall takes it away",
		"The rota daemon that was running is stopped, and its sessions keep running; rota serve start starts one again",
	} {
		if code != 1 || !strings.Contains(errb, want) {
			t.Fatalf("a reinstall that fails: exit %d %q, want %q", code, errb, want)
		}
	}
	select {
	case <-termed:
	default:
		t.Fatal("the daemon running was not sent SIGTERM")
	}
}

// rota serve install and uninstall, like serve stop, are refused inside a
// held session: either may end the daemon, and uninstall take away what
// starts it again, from inside one by mistake.
// Once the session is over, the same process may. The
// probe is this binary as a rota in a session of its own, which a held
// session's record names as its holder: sessionProbe, which serve stop's
// test drives too.
func TestInstallAndUninstallAreRefusedInsideAHeldSession(t *testing.T) {
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

	for _, verb := range []string{"install", "uninstall"} {
		code, said := ask("serve " + verb)
		if want := "rota serve " + verb + " is refused inside held session " + id + "; run it from outside the session"; code != 1 || !strings.Contains(said, want) {
			t.Fatalf("%s inside a held session: exit %d %q, want %q", verb, code, said, want)
		}
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
	// A new path, which macOS looks at before it lets anything run from it.
	if err := fakecli.Warm(link); err != nil {
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
// and the unit names none. A CLI an older rota launched has ROTA_ACCOUNT_ID
// and no ROTA_CLAUDE_HOME: its CLAUDE_CONFIG_DIR is the mirror too, and the
// unit names none either.
func TestTheUnitCarriesThePersonsClaudeDirectory(t *testing.T) {
	r := newUnitRig(t)
	exe, err := invokedAs()
	if err != nil {
		t.Fatal(err)
	}
	mirror, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", mirror)
	t.Chdir(cwd)
	for _, c := range []struct {
		home, account string
		set           bool
		want          string
	}{
		{filepath.Join(cwd, "real"), "", true, filepath.Join(cwd, "real")},
		{"real", "", true, filepath.Join(cwd, "real")},
		{"", "", true, ""},
		{"", "1", false, ""},
	} {
		if c.set {
			t.Setenv("ROTA_CLAUDE_HOME", c.home)
		} else {
			t.Setenv("ROTA_CLAUDE_HOME", "")
			os.Unsetenv("ROTA_CLAUDE_HOME")
		}
		t.Setenv("ROTA_ACCOUNT_ID", c.account)
		if _, errb, code := call(t, "serve", "install"); code != 0 {
			t.Fatalf("install with ROTA_CLAUDE_HOME=%q (set %v), ROTA_ACCOUNT_ID=%q: exit %d %q", c.home, c.set, c.account, code, errb)
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
			t.Fatalf("ROTA_CLAUDE_HOME=%q (set %v), ROTA_ACCOUNT_ID=%q: unit:\n%s\nwant:\n%s", c.home, c.set, c.account, got, want)
		}
		if strings.Contains(string(raw), mirror) {
			t.Fatalf("ROTA_CLAUDE_HOME=%q (set %v), ROTA_ACCOUNT_ID=%q: the account's mirror reached the unit", c.home, c.set, c.account)
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
