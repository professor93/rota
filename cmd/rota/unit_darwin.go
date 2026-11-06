//go:build darwin

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// unitFile is where this store's launchd agent lives.
func unitFile(store string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	_, label := unitName(store)
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// writeUnit writes the agent and bootstraps it. It does not look for a
// daemon started by hand; the command wiring does.
func writeUnit(exe, store string, env []string) (string, error) {
	path, err := unitFile(store)
	if err != nil {
		return "", err
	}
	_, label := unitName(store)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(launchdPlist(label, exe, store, env)), 0o644); err != nil {
		return "", err
	}
	return path, unitRun("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), path)
}

// removeUnit boots the agent out and deletes its file. Sessions keep running:
// the plist has AbandonProcessGroup.
func removeUnit(store string) error {
	path, err := unitFile(store)
	if err != nil {
		return err
	}
	_, label := unitName(store)
	if err := unitRun("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
