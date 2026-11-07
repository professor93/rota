//go:build unix

package main

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/professor93/rota/store"
)

// The handover replaces rota with the CLI, and the claim on the account has
// to go with it: the CLI is the run from then on. Where no holder can be
// started to hold it (see holdClaims), the claim is kept across the exec, as
// it always was. Here the replacing is a child started the way an exec keeps
// descriptors, and once the handover has let go of its own copy the account
// must still be claimed — by the CLI. A handover that stopped passing the
// claim on would leave it free.
func TestWithoutAHolderTheHandoverPassesTheClaimToTheCLI(t *testing.T) {
	seedLiving(t, "")
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	var cli *exec.Cmd
	execProcess = func(string, []string, []string) error {
		cli = exec.Command("/bin/sleep", "5")
		return cli.Start()
	}
	if _, errOut, code := call(t, "run", "1"); code != 0 {
		t.Fatalf("%d %q", code, errOut)
	}
	if cli == nil || cli.Process == nil {
		t.Fatal("the handover never happened")
	}
	t.Cleanup(func() { _ = cli.Process.Kill(); _ = cli.Wait() })
	s, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.InUse(s.Find(1)) {
		t.Fatal("the CLI the handover became holds the account's claim")
	}
}

// A handover that panics on its way to the CLI lets go of the claim it took,
// and the panic goes on as it was: only a claim handed to the CLI outlives
// the handover.
func TestAHandoverThatPanicsLetsGoOfTheClaim(t *testing.T) {
	seedLiving(t, "")
	orig := execProcess
	t.Cleanup(func() { execProcess = orig })
	boom := errors.New("a handover panicking on purpose")
	execProcess = func(string, []string, []string) error { panic(boom) }
	func() {
		defer func() {
			if v := recover(); v != boom {
				t.Fatalf("the panic reaches the caller, as it was: %v", v)
			}
		}()
		call(t, "run", "1")
	}()
	s, err := store.Open("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.InUse(s.Find(1)) {
		t.Fatal("the account is let go of on the way out")
	}
}
