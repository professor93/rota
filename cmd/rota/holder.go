package main

import (
	"errors"
	"strconv"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// An interactive run replaces rota with the CLI, and the account's claim has
// to outlive the replacing: the CLI is the run from then on. Kept across the
// exec, the claim went to everything the CLI started as well, and stayed
// with whatever of that outlived it — Codex's app-server daemon, which its
// TUI starts and leaves running, or on macOS a tool a codex agent left
// running in the background — so the account read busy, to every run, to the
// rotation and to `rota remove`, until the last of them had gone.
//
// So the handover lends its claim to a holder instead, `rota serve claim
// <pid> <count>`: a process in a session of its own, at /, with nothing open
// but /dev/null and what it was lent, started before the exec and belonging
// to init rather than to the CLI. It holds the claim through its own
// descriptor, close-on-exec as every one of rota's is, and lets go of it the
// moment the process it was given exits: rota's own, which after the exec is
// the CLI's. Nothing the CLI starts holds the claim, and the CLI's process,
// terminal, signals, job control and exit status are what they were. Where
// no holder can be started, the claim is kept across the exec as before.

// errNoHolder says a holder is not to be had here, rather than that starting
// one went wrong: the claim is kept across the exec, and nothing is said.
var errNoHolder = errors.New("no claim holder on this platform")

// claimHolder is a holder that has the claim and is watching this process.
type claimHolder interface {
	// letGo tells the holder to let go now, because the exec did not
	// happen, and returns once it has. It is never called after an exec
	// that worked: there is no rota left to call it.
	letGo()
}

// keptAcrossExec is the claim kept across the exec, where no holder was
// started: there is nothing to tell, and this process's release lets go.
type keptAcrossExec struct{}

func (keptAcrossExec) letGo() {}

// startHolder starts a holder for the claims, lent as files, and returns once
// it has them; a variable so a test can see the handover without starting
// this test binary again.
var startHolder = holdInBackground

// holdClaims gives the claims this process holds to a holder, or keeps them
// across the exec where it cannot. It is called immediately before the exec,
// after every other process the handover starts, so none of those is given a
// claim either.
func (c *cli) holdClaims(a *rota.Account) claimHolder {
	files := store.ClaimFiles()
	if len(files) == 0 {
		return keptAcrossExec{}
	}
	h, err := startHolder(files)
	if err == nil {
		return h
	}
	if !errors.Is(err, errNoHolder) {
		warn(c.err, "could not start the process that holds %s's claim while it runs, so the CLI holds it, "+
			"and anything the CLI leaves running keeps the account busy: %v", a, err)
	}
	store.KeepClaimsAcrossExec()
	return keptAcrossExec{}
}

// serveClaim is `rota serve claim <pid> <count>`: started by an interactive
// run, it holds the claims the run lent it until process pid exits. It is
// never typed: what it holds are the descriptors it was started with.
func (c *cli) serveClaim(args []string) error {
	if len(args) != 2 {
		return usageErr("serve claim is internal: an interactive rota run starts it")
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 0 {
		return usageErr("serve claim is internal: an interactive rota run starts it")
	}
	n, err := strconv.Atoi(args[1])
	if err != nil || n < 1 || n > maxLent {
		return usageErr("serve claim is internal: an interactive rota run starts it")
	}
	return holdLent(pid, n)
}

// maxLent bounds how many claims one holder is lent. A run holds one; the
// bound only keeps a mistyped count from naming every descriptor there is.
const maxLent = 64
