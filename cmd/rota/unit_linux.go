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
	if err := supervise("systemctl --user daemon-reload"); err != nil {
		return path, err
	}
	if !again {
		return path, supervise("systemctl --user enable --now " + name)
	}
	if err := supervise("systemctl --user enable " + name); err != nil {
		return path, err
	}
	return path, supervise("systemctl --user restart " + name)
}

// removeUnit stops and disables the unit and deletes its file. Sessions keep
// running: the unit has KillMode=process.
func removeUnit(store string) error {
	path, err := unitFile(store)
	if err != nil {
		return err
	}
	name, _ := unitName(store)
	serr := supervise("systemctl --user disable --now " + name)
	rerr := os.Remove(path)
	if os.IsNotExist(rerr) {
		rerr = nil
	}
	return errors.Join(serr, rerr, supervise("systemctl --user daemon-reload"))
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
