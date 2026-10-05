//go:build unix

package main

import (
	"os/exec"
	"testing"

	"github.com/professor93/rota/store"
)

// The handover replaces rota with the CLI, and the claim on the account has
// to go with it: the CLI is the run from then on. Here the replacing is a
// child started the way an exec keeps descriptors, and once the handover has
// let go of its own copy the account must still be claimed — by the CLI. A
// handover that stopped passing the claim on would leave it free.
func TestTheHandoverPassesTheClaimToTheCLI(t *testing.T) {
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
