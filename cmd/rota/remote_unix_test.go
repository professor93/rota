//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/fakecli"
)

// In a person's own terminal the CLI's own arguments are handed over
// untouched, Remote Control among them. From inside a held session they are
// not: that is most likely the agent in one, which may have been started
// from another device, so Remote Control asked for in the CLI's own
// arguments is refused there in one line, the way rota serve stop is, and
// nothing is handed over. Asking for nothing more is handed over as before,
// and once the session is over the same arguments are the person's again.
func TestRemoteControlInTheCLIsOwnArgsIsRefusedInsideAHeldSession(t *testing.T) {
	rotaHome, _ := seedLiving(t, "")
	handedTo := handover(t)
	const id = "00000000000000f1"
	release := heldRecord(t, filepath.Join(rotaHome, "terminals"), id, os.Getpid())
	want := "Remote Control in the CLI's own arguments is refused inside held session " + id +
		"; run it from outside the session, or ask for it with rota's own --remote-control, before --"
	for _, args := range [][]string{
		{"run", "1", "--", "--rc"},
		{"run", "1", "-i", "remote-control"},
		{"run", "1", "--", "--settings", `{"remoteControlAtStartup":true}`},
		// One rota cannot read counts as asking there, with the directory
		// readable.
		{"run", "1", "--", "--settings", "missing.json"},
		{"run", "1", "--", "--managed-settings", `{"remoteControlAtStartup":true}`},
	} {
		_, errOut, code := call(t, args...)
		if code == 0 || !strings.Contains(errOut, want) || len(*handedTo) != 0 {
			t.Fatalf("rota %s inside a held session: exit %d %q, handed %v; want it refused with %q", strings.Join(args, " "), code, errOut, *handedTo, want)
		}
	}
	if _, errOut, code := call(t, "run", "1", "--", "--model", "opus"); code != 0 || !slices.Contains(*handedTo, "opus") {
		t.Fatalf("arguments that ask for nothing inside a held session: exit %d %q, handed %v", code, errOut, *handedTo)
	}
	release()
	*handedTo = nil
	if _, errOut, code := call(t, "run", "1", "--", "--rc"); code != 0 || len(*handedTo) == 0 || (*handedTo)[len(*handedTo)-1] != "--rc" {
		t.Fatalf("outside every held session: exit %d %q, handed %v; want --rc handed over untouched", code, errOut, *handedTo)
	}
	if storedAccount(t, rotaHome)["remoteControl"] == true {
		t.Fatal("the person's own --rc turned the account's setting on")
	}
}

// A foreground rota run at this machine's own terminal is the person's, so
// it is handed over as it always was: nobody else can type into it, and no
// guard against Remote Control is added, with the account's setting off.
func TestAForegroundRunCarriesNoGuard(t *testing.T) {
	rotaHome, _ := seedLiving(t, "")
	handedTo := handover(t)
	for _, args := range [][]string{{"run", "1"}, {"run", "1", "--", "--model", "opus"}, {"run", "1", "-i"}} {
		if _, errOut, code := call(t, args...); code != 0 {
			t.Fatalf("rota %s: exit %d %q", strings.Join(args, " "), code, errOut)
		}
		for _, a := range *handedTo {
			if strings.Contains(a, "disableRemoteControl") || a == "--settings" {
				t.Fatalf("rota %s handed %q, want no guard", strings.Join(args, " "), *handedTo)
			}
		}
	}
	if storedAccount(t, rotaHome)["remoteControl"] == true {
		t.Fatal("the account's setting was turned on")
	}
	// Nor does a question asked here, claude -p, which the person's own
	// terminal starts.
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Result(0))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, errOut, code := call(t, "run", "1", "-p", "hi")
	if code != 0 || !strings.Contains(out, "ARGS=") || strings.Contains(out, "disableRemoteControl") || strings.Contains(out, "--settings") {
		t.Fatalf("rota run 1 -p hi: exit %d %q %q, want the question asked with no guard", code, out, errOut)
	}
}

// A directory that cannot be read — one removed from under the caller, say —
// is how an agent could step out of the check, so it is taken as inside a
// held session: Remote Control in the CLI's own arguments is refused there,
// held session or not, while arguments that ask for nothing are handed over
// as before. macOS still names a removed directory, so the failure is
// stood in for.
func TestRemoteControlInTheCLIsOwnArgsIsRefusedWhereTheDirectoryIsGone(t *testing.T) {
	seedLiving(t, "")
	handedTo := handover(t)
	was := getwd
	t.Cleanup(func() { getwd = was })
	getwd = func() (string, error) { return "", os.ErrNotExist }
	want := "Remote Control in the CLI's own arguments is refused when the current directory cannot be read, as it is " +
		"inside a held session; run it from a directory that exists, or ask for it with rota's own --remote-control, before --"
	for _, args := range [][]string{{"run", "1", "--", "--rc"}, {"run", "1", "--", "--settings", "rc.json"}} {
		if _, errOut, code := call(t, args...); code == 0 || !strings.Contains(errOut, want) || len(*handedTo) != 0 {
			t.Fatalf("rota %s from a removed directory: exit %d %q, handed %v; want %q", strings.Join(args, " "), code, errOut, *handedTo, want)
		}
	}
	if _, errOut, code := call(t, "run", "1", "--", "--model", "opus"); code != 0 || !slices.Contains(*handedTo, "opus") {
		t.Fatalf("arguments that ask for nothing from a removed directory: exit %d %q, handed %v", code, errOut, *handedTo)
	}
}
