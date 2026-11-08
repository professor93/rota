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

// bootoutNotLoaded is launchctl bootout's exit status for a label nothing
// is loaded under: "Boot-out failed: 3: No such process".
const bootoutNotLoaded = 3

// exitedWith says whether err is a supervisor command that ran and failed
// with exit status code: runSupervisor's wraps an *exec.ExitError, whose
// ExitCode says it, and a test's stand-in answers the same.
func exitedWith(err error, code int) bool {
	var e interface{ ExitCode() int }
	return errors.As(err, &e) && e.ExitCode() == code
}

// stopUnit boots the agent out. Sessions keep running: the plist has
// AbandonProcessGroup. An agent not loaded — booted out by hand, or never
// bootstrapped — is already what bootout asks for, so that answer is not an
// error; any other failure is.
func stopUnit(store string) error {
	_, label := unitName(store)
	err := unitRun("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), label))
	if exitedWith(err, bootoutNotLoaded) {
		return nil
	}
	return err
}

// deleteUnit deletes the agent's file. launchd reads it only to load the
// agent, so there is nothing to tell it.
func deleteUnit(store string, _ bool) error {
	path, err := unitFile(store)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// keptWhy is why an agent whose daemon is still running keeps its file, and
// keptFrom where uninstall can reach launchd instead.
const (
	keptWhy  = "launchd still has the agent loaded"
	keptFrom = "a login in the Mac's GUI session"
)

// lingerHint is nothing here: a launchd agent starts at login on its own.
func lingerHint() string { return "" }
