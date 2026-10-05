// Package claudecode is what rota does to a Claude Code configuration
// directory that is not reading or writing a file in it: finding and
// removing the macOS keychain item that holds its login, and stopping its
// daemon.
//
// Both are other programs — /usr/bin/security and claude itself — so both
// are reached through a function variable, and every test in the module
// that could reach them replaces it. The real runners also run nothing
// inside a test binary — the keychain answers that it holds no item, and
// stopping a daemon fails — so a test that forgot to stand in for them, or a
// test of a program built on rota, which cannot reach this package at all,
// never reads a real keychain or stops a real daemon: those are a person's
// login and a person's background work, and no test is worth either.
package claudecode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Security runs /usr/bin/security with these arguments and reports what it
// printed and how it exited. A variable so tests stand in for the keychain.
var Security = runSecurity

// StopDaemon stops the Claude Code daemon of one configuration directory,
// and with it the background sessions it hosts, by running
// `claude daemon stop --any` with the given environment. It is bounded by
// ctx. A variable so tests stand in for Claude Code.
var StopDaemon = runDaemonStop

// Unreachable is an address no request reaches: what a test binary points
// the provider's endpoints at until a test installs a fake, so a call nobody
// stood in for fails here instead of reaching the real service with a test's
// made-up token.
const Unreachable = "http://127.0.0.1:1/unreachable"

// StandIn replaces both runners with ones that touch nothing: the keychain
// holds no item, and stopping a daemon fails. Every TestMain that can reach
// a claude launch calls it, and a test that wants more installs its own.
func StandIn() {
	Security = func(context.Context, ...string) ([]byte, int, error) { return nil, Absent, nil }
	StopDaemon = func(context.Context, string, []string) error {
		return errors.New("this test did not stand in for stopping a daemon")
	}
}

// errInTest is what the real runners answer inside a test binary.
var errInTest = errors.New("refusing to run a real program against real state from a test binary; replace the runner")

func inTest() bool { return testing.Testing() }

// Absent is the exit status security gives when no such item exists.
const Absent = 44

// ServicePrefix begins every keychain item Claude Code keeps a login in.
const ServicePrefix = "Claude Code-credentials-"

// Service is the keychain item name Claude Code uses for a configuration
// directory: the prefix and the first eight hex digits of the sha256 of the
// directory exactly as CLAUDE_CONFIG_DIR exports it, after Unicode NFC
// normalisation. ok is false for a directory that is not pure ASCII,
// because normalising it would take tables the standard library does not
// have, and a wrong name is worse than none: it would be a different item.
func Service(dir string) (name string, ok bool) {
	for i := 0; i < len(dir); i++ {
		if dir[i] >= 0x80 {
			return "", false
		}
	}
	sum := sha256.Sum256([]byte(dir))
	return ServicePrefix + hex.EncodeToString(sum[:])[:8], true
}

var plainUser = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// Account is the keychain account Claude Code files the item under: $USER
// when it is a plain name, else the operating system's name for this user,
// else a fixed word.
func Account() string {
	if u := os.Getenv("USER"); plainUser.MatchString(u) {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "claude-code-user"
}

// Read returns the keychain item's content — Claude Code's credential store
// as JSON — and whether there is one.
func Read(ctx context.Context, service string) ([]byte, bool, error) {
	out, code, err := Security(ctx, "find-generic-password", "-a", Account(), "-w", "-s", service)
	switch {
	case err != nil:
		return nil, false, err
	case code == Absent:
		return nil, false, nil
	case code != 0:
		return nil, false, &ExitError{Op: "find-generic-password", Code: code}
	}
	// security prints the secret followed by one newline, which is not part
	// of it.
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, true, nil
}

// Delete removes the keychain item. An item that is not there is already
// deleted.
func Delete(ctx context.Context, service string) error {
	_, code, err := Security(ctx, "delete-generic-password", "-a", Account(), "-s", service)
	switch {
	case err != nil:
		return err
	case code != 0 && code != Absent:
		return &ExitError{Op: "delete-generic-password", Code: code}
	}
	return nil
}

// ExitError is security refusing in a way that is not "no such item".
type ExitError struct {
	Op   string
	Code int
}

func (e *ExitError) Error() string {
	return "security " + e.Op + " exited with status " + strconv.Itoa(e.Code)
}

// runDaemonStop is StopDaemon for real. Its output is kept only to explain
// a failure: whatever Claude Code printed is the one account of why.
func runDaemonStop(ctx context.Context, home string, env []string) error {
	if inTest() {
		return errInTest
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, path, "daemon", "stop", "--any")
	cmd.Env = env
	cmd.Dir = home
	out, err := cmd.CombinedOutput()
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
	}
	return err
}
