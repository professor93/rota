//go:build linux

package main

import (
	"errors"
	"os"
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

// writeUnit writes the unit and has systemd enable and start it. It does not
// look for a daemon started by hand; the command wiring does.
func writeUnit(exe, store string, env []string) (string, error) {
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
	return path, supervise("systemctl --user enable --now " + name)
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
