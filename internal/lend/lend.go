// Package lend carries one thing from the store to the program's handover
// that is nobody else's business, and so is no part of the store's API:
// copies of the claims this process holds, for a handover that lends them to
// a process of its own to hold while the CLI runs (cmd/rota's `rota serve
// claim`), rather than keeping them across its exec. And it says whose CLI
// is kept from its claim that way (Lends), for that handover and for a held
// session's holder alike.
//
// The store registers the function at init; a program that imports the
// store, as any handover does, has it.
package lend

import (
	"os"

	rota "github.com/professor93/rota/lib"
)

var claims func() ([]*os.File, error)

// Register is the store's, called once at init.
func Register(f func() ([]*os.File, error)) { claims = f }

// Claims returns a close-on-exec copy of the file of every claim this process
// holds: the same open file, so a process given one holds the claim through
// its own descriptor — a flock belongs to the open file, and is let go of
// when the last descriptor of it is closed. The copies are the caller's to
// pass on and to close; closing one lets go of nothing while the store still
// holds its own. None are returned where nothing can be lent, Windows among
// those, and an error when a claim held cannot be: lending some and not the
// others would let the others go at the exec.
func Claims() ([]*os.File, error) {
	if claims == nil {
		return nil, nil
	}
	return claims()
}

// Lends reports whether the CLI of an account of provider is kept from the
// account's claim, which a process of rota's that lives exactly as long as
// the CLI holds beside it instead: an interactive run lends it to `rota serve
// claim`, and a held session's holder keeps its own. It is so for a CLI known
// to leave a daemon running that holds what the CLI inherited, which would
// keep the account claimed after the CLI had gone. Codex's does, and so does
// any provider that drives Codex's CLI. Claude Code's daemon was seen holding
// no claim of rota's, and a claude account reads in use while any claude
// process lives in its home besides, so a claude CLI is given its claim, by
// an interactive run and by a holder alike.
func Lends(provider string) bool { return rota.Flavor(provider) == "codex" }
