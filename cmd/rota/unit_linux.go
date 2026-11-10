//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
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

// systemctl asks the user's systemd, one argv as launchctl is asked.
func systemctl(args ...string) error {
	return unitRun("systemctl", append([]string{"--user"}, args...)...)
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

// keptWhy is why a unit whose daemon is still running keeps its file, and
// keptFrom where uninstall can reach systemd instead. As measured: systemd,
// reloading a unit whose file is gone, loads it with no KillMode=process,
// and the daemon then stopping ends every session in the unit.
const (
	keptWhy  = "with it gone, systemd would end the unit's sessions when the daemon stops"
	keptFrom = "a login that has a user bus"
)

// Where a user manager shows itself without the bus. Variables so a test
// can point them elsewhere.
var (
	procDir   = "/proc"
	cgroupDir = "/sys/fs/cgroup"
)

// restartUnknown is why, with the bus not to be reached, a unit whose
// daemon is not running keeps its file: after a daemon fails, systemd waits
// RestartSec, 5 seconds, and starts the unit again from what it loaded, and
// whether it is waiting so is known to systemd alone: the timer is in its
// memory, not in a file under /run or the cgroup tree, where a unit that
// stopped for good looks the same. A daemon started then, under a unit
// whose file is gone, is the one the next reload strips of KillMode=process.
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
