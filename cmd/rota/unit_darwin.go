//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
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

// bootstrapRetry is how long install waits between tries at bootstrapping
// an agent it has just booted out, which launchd may still be letting go
// of. A variable so a test need not wait.
var bootstrapRetry = 200 * time.Millisecond

// writeUnit writes the agent and bootstraps it. An agent still loaded under
// its label — this one, installed before, or one whose plist was taken away
// by hand — makes bootstrap fail with 5, so it is booted out first, which
// also restarts one rewritten; nothing loaded is the usual answer to that,
// and not an error. It does not look for a daemon started by hand; the
// command does. The path is returned once the file is written, with any
// error the supervisor gave.
func writeUnit(exe, store string, env []string, _ bool) (string, error) {
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
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	unloaded := unitRun("launchctl", "bootout", domain+"/"+label) == nil
	err = unitRun("launchctl", "bootstrap", domain, path)
	for try := 1; err != nil && unloaded && try < 10; try++ {
		time.Sleep(bootstrapRetry)
		err = unitRun("launchctl", "bootstrap", domain, path)
	}
	return path, err
}

// removeUnit boots the agent out and deletes its file. Sessions keep running:
// the plist has AbandonProcessGroup.
func removeUnit(store string) error {
	path, err := unitFile(store)
	if err != nil {
		return err
	}
	_, label := unitName(store)
	serr := unitRun("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	rerr := os.Remove(path)
	if os.IsNotExist(rerr) {
		rerr = nil
	}
	return errors.Join(serr, rerr)
}

// lingerHint is nothing here: a launchd agent starts at login on its own.
func lingerHint() string { return "" }
