//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// An agent install booted out may still be on its way out when it is
// bootstrapped again, so a bootstrap that fails after a bootout that
// unloaded something is tried again; one that fails with nothing booted
// out is said at once.
func TestABootstrapAfterABootoutIsTriedAgain(t *testing.T) {
	old := bootstrapRetry
	bootstrapRetry = time.Millisecond
	t.Cleanup(func() { bootstrapRetry = old })
	home := unitHome(t, true)
	store := filepath.Join(home, ".rota")
	path := filepath.Join(home, "Library", "LaunchAgents", "com.github.professor93.rota.plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	bootout := "launchctl bootout " + domain + "/com.github.professor93.rota"
	bootstrap := "launchctl bootstrap " + domain + " " + path

	failures := 2
	calls := recordUnits(t, func(call string) error {
		if call == bootstrap && failures > 0 {
			failures--
			return errors.New("Bootstrap failed: 5: Input/output error")
		}
		return nil
	})
	if _, err := writeUnit("/u/rota", store, nil, true); err != nil {
		t.Fatalf("a bootstrap that succeeds on its third try: %v", err)
	}
	if want := []string{bootout, bootstrap, bootstrap, bootstrap}; !slices.Equal(*calls, want) {
		t.Fatalf("asked %q, want %q", *calls, want)
	}

	calls = recordUnits(t, func(call string) error {
		return errors.New("Bootstrap failed: 5: Input/output error")
	})
	if _, err := writeUnit("/u/rota", store, nil, false); err == nil {
		t.Fatal("a bootstrap that fails is not an error")
	}
	if want := []string{bootout, bootstrap}; !slices.Equal(*calls, want) {
		t.Fatalf("with nothing booted out asked %q, want %q", *calls, want)
	}
}

// exitStatus is a supervisor command's failure with an exit status, as an
// *exec.ExitError says one: in its words, and by ExitCode.
type exitStatus int

func (s exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(s)) }
func (s exitStatus) ExitCode() int { return int(s) }

// bootoutFails has launchctl bootout fail as runSupervisor says it does —
// the command, its exit status and what it said — and every other call
// succeed, and writes down each call.
func bootoutFails(t *testing.T, status int, said string) *[]string {
	t.Helper()
	return recordUnits(t, func(call string) error {
		if strings.HasPrefix(call, "launchctl bootout ") {
			return fmt.Errorf("%s: %w: %s", call, exitStatus(status), said)
		}
		return nil
	})
}

// An agent that is not loaded — booted out by hand, say — is already what
// uninstall asks for: bootout's exit status 3, "No such process", is not
// an error, and the plist is deleted as always.
func TestUninstallingAnAgentNotLoadedIsNotAnError(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	calls := bootoutFails(t, 3, "Boot-out failed: 3: No such process")
	out, errb, code := call(t, "serve", "uninstall")
	if code != 0 || errb != "" || out != fmt.Sprintf("rota daemon uninstalled for %s; 0 sessions keep running\n", r.store) {
		t.Fatalf("uninstall of an agent not loaded: exit %d %q %q", code, out, errb)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("plist still there: %v", err)
	}
	if !slices.Equal(*calls, r.want.uninstall) {
		t.Fatalf("asked %q, want %q", *calls, r.want.uninstall)
	}
}

// Any other bootout failure is still the error, in launchctl's words, and
// the plist is deleted all the same.
func TestUninstallSaysAnyOtherBootoutFailure(t *testing.T) {
	r := newUnitRig(t)
	if _, errb, code := call(t, "serve", "install"); code != 0 {
		t.Fatalf("install: exit %d %q", code, errb)
	}
	bootoutFails(t, 5, "Boot-out failed: 5: Input/output error")
	out, errb, code := call(t, "serve", "uninstall")
	want := fmt.Sprintf("%s is removed, but %s: exit status 5: Boot-out failed: 5: Input/output error", r.want.path, r.want.uninstall[0])
	if code != 1 || out != "" || !strings.Contains(errb, want) {
		t.Fatalf("uninstall with bootout failing 5: exit %d %q %q, want %q", code, out, errb, want)
	}
	if _, err := os.Lstat(r.want.path); !os.IsNotExist(err) {
		t.Fatalf("plist still there: %v", err)
	}
}

// A command runSupervisor ran is read by its exit status as the stand-in
// is, and words alone are no status.
func TestASupervisorsExitStatusIsRead(t *testing.T) {
	err := runSupervisor("false")
	if !exitedWith(err, 1) || exitedWith(err, bootoutNotLoaded) {
		t.Fatalf("false: %v is not read as exit status 1", err)
	}
	if exitedWith(errors.New("exit status 3: Boot-out failed: 3: No such process"), bootoutNotLoaded) {
		t.Fatal("an error with no exit status is read as one")
	}
}
