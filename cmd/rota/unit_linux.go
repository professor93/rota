//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func unitDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// unitFile is where this store's systemd user unit lives.
func unitFile(store string) (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	name, _ := unitName(store)
	return filepath.Join(dir, name), nil
}

// systemctl asks the user's systemd, one argv as launchctl is asked. From
// sudo -u, su or cron there is no XDG_RUNTIME_DIR that reaches a manager —
// none, the invoker's, or one with no manager in it — and systemctl --user
// then finds no bus; so where this user's manager listens on its private
// socket in its own runtime directory, systemctl is given that directory,
// which it reaches the manager through with no bus at all. It is the one
// manager of this uid, user@<uid>.service, which the unit was installed
// with. Only a setup that runs a second manager at a runtime directory of
// its own would differ.
func systemctl(args ...string) error {
	argv := append([]string{"--user"}, args...)
	if dir := managerRuntimeDir(); dir != "" {
		return unitRun("XDG_RUNTIME_DIR="+dir, append([]string{"systemctl"}, argv...)...)
	}
	return unitRun("systemctl", argv...)
}

// runtimeBase is where logind makes each user's runtime directory. A
// variable so a test can point it elsewhere.
var runtimeBase = "/run/user"

// managerRuntimeDir is the runtime directory systemctl is to be given, or
// "" when it is to be left as it is: XDG_RUNTIME_DIR already names one with
// a manager's socket at systemd/private, or runtimeBase/<uid> is not this
// user's own directory with this user's own socket there. The one named is
// judged by its socket alone, whoever's it is: one this process cannot
// reach does not stat, and root given a user's, as sudo -E gives it, reaches
// that user's manager, which has the unit, and not root's own.
func managerRuntimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		if fi, err := os.Stat(filepath.Join(d, "systemd", "private")); err == nil && fi.Mode().Type() == fs.ModeSocket {
			return ""
		}
	}
	uid := os.Getuid()
	dir := filepath.Join(runtimeBase, strconv.Itoa(uid))
	if !ownedBy(dir, uid, fs.ModeDir) || !ownedBy(filepath.Join(dir, "systemd", "private"), uid, fs.ModeSocket) {
		return ""
	}
	return dir
}

// ownedBy says whether path is of that kind and uid's.
func ownedBy(path string, uid int, kind fs.FileMode) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().Type() == kind && ownerOf(fi) == uid
}

// ownerOf is the uid that owns what fi describes, or -1. A variable so a
// test can have something be somebody else's without being root.
var ownerOf = func(fi fs.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return -1
}

// daemonUnderUnit says whether the daemon whose pid this is is the one the
// store's unit started. A variable so a test can say either.
var daemonUnderUnit = underUnit

// underUnit says whether pid's cgroup, on either layout, is the store's
// unit's, as /proc says. One that cannot be read is not taken for it. A
// daemon started by hand from inside the unit's cgroup — from a session
// the unit's daemon held, which stays there after that daemon is gone — is
// taken for the unit's own: nothing on the outside tells the two apart.
func underUnit(store string, pid int) bool {
	raw, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return false
	}
	name, _ := unitName(store)
	for _, line := range strings.Split(string(raw), "\n") {
		if f := strings.SplitN(line, ":", 3); len(f) == 3 && path.Base(f[2]) == name {
			return true
		}
	}
	return false
}

// writeUnit writes the unit and has systemd enable and start it. again says
// the unit was there before: enable --now leaves a unit that runs as it was
// started, so a rewritten one is restarted to take up what changed. It does
// not look for a daemon started by hand; the command does. The path is
// returned once the file is written, with any error the supervisor gave.
func writeUnit(exe, store string, env []string, again bool) (string, error) {
	path, err := unitFile(store)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(systemdUnit(exe, store, env)), 0o644); err != nil {
		return "", err
	}
	name, _ := unitName(store)
	if err := systemctl("daemon-reload"); err != nil {
		return path, err
	}
	if !again {
		return path, systemctl("enable", "--now", name)
	}
	if err := systemctl("enable", name); err != nil {
		return path, err
	}
	return path, systemctl("restart", name)
}

// stopUnit has systemd stop and disable the unit. Sessions keep running: the
// unit has KillMode=process.
func stopUnit(store string) error {
	name, _ := unitName(store)
	return systemctl("disable", "--now", name)
}

// deleteUnit deletes the unit's file, and the link enable made for it in
// default.target.wants: a disable that failed — with no user bus to reach,
// say — leaves that link naming a file that is gone, which no later
// uninstall could find again. With reload, systemd is then told; without,
// it forgets the unit at its next reload.
func deleteUnit(store string, reload bool) error {
	path, err := unitFile(store)
	if err != nil {
		return err
	}
	name, _ := unitName(store)
	var lerr error
	wants := filepath.Join(filepath.Dir(path), "default.target.wants", name)
	if fi, err := os.Lstat(wants); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		lerr = os.Remove(wants)
	}
	rerr := os.Remove(path)
	if os.IsNotExist(rerr) {
		rerr = nil
	}
	if !reload {
		return errors.Join(lerr, rerr)
	}
	return errors.Join(lerr, rerr, systemctl("daemon-reload"))
}

// keptWhy is why a unit whose daemon is still running keeps its file. As
// measured: systemd, reloading a unit whose file is gone, loads it with no
// KillMode=process, and the daemon then stopping ends every session in the
// unit.
const keptWhy = "with it gone, systemd would end the unit's sessions when the daemon stops"

// againFrom is what to do, after a unit was kept because systemctl failed
// saying serr: where systemctl could not reach the manager, uninstall
// again from a login of the person's own, which has the manager's runtime
// directory, or from where systemctl is given it; otherwise once systemctl
// can do what it was asked.
func againFrom(serr error) string {
	if s := serr.Error(); strings.Contains(s, "Failed to connect to") && strings.Contains(s, "bus") {
		return fmt.Sprintf("run rota serve uninstall again from an ssh or console login of yours, or from sudo -u, su or cron while your user manager listens at %s",
			filepath.Join(runtimeBase, strconv.Itoa(os.Getuid()), "systemd", "private"))
	}
	return "run rota serve uninstall again once systemctl --user can stop the unit"
}

// Where a user manager shows itself without the bus. Variables so a test
// can point them elsewhere.
var (
	procDir   = "/proc"
	cgroupDir = "/sys/fs/cgroup"
)

// restartUnknown is why, with systemd not to be asked, a unit whose own
// daemon is not running keeps its file: after a daemon fails, systemd waits
// RestartSec, 5 seconds, and starts the unit again from what it loaded, and
// whether it is waiting to, only systemd can say, over its bus. A daemon
// started then, under a unit whose file is gone, is the one the next reload
// strips of KillMode=process.
const restartUnknown = "systemd may be about to start one again, as it does 5 seconds after one fails, which only systemd can tell"

// restartMayCome says why systemd may yet start the unit's daemon again
// when uninstall could not reach it, or "" when no user manager runs to do
// it. A variable so a test can say either.
var restartMayCome = func() string {
	if userManagerRuns(procDir, cgroupDir, os.Getuid()) {
		return restartUnknown
	}
	return ""
}

// lingerDir is where systemd records the users whose units start at boot
// and outlive their logins. A variable so a test can point it elsewhere.
var lingerDir = "/var/lib/systemd/linger"

// lingerHint is what to run so that this user's units start at boot and
// outlive the login, or nothing when systemd records that already, or when
// who this user is cannot be told.
func lingerHint() string {
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	if name == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(lingerDir, name)); !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return fmt.Sprintf("run %q so the daemon starts at boot and outlives your login", "loginctl enable-linger "+name)
}
