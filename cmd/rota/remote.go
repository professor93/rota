package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// remoteAsk is `rota run --remote-control[=name]`: open the account's Claude
// Code with its own Remote Control on, under that name when one is given.
type remoteAsk struct {
	on   bool
	name string
}

// remoteWithPrompt is why a question cannot have Remote Control: there is
// no session left afterwards for anybody to control.
const remoteWithPrompt = "--remote-control opens Claude Code's own session with Remote Control on, so it takes no prompt: " +
	"a question is answered and over, and leaves nothing to control"

// takeRemoteControl lifts --remote-control out of the arguments before
// anything else reads them, as --share is. The name comes only after an
// equals sign: a bare --remote-control followed by a word could otherwise be
// a name or the start of a prompt, and rota does not guess which.
func takeRemoteControl(args []string) ([]string, remoteAsk, error) {
	var ask remoteAsk
	out := make([]string, 0, len(args))
	for i, a := range args {
		if a == "--" {
			return append(out, args[i:]...), ask, nil
		}
		name, value, hasValue := strings.Cut(a, "=")
		if name == "--remote-control" {
			ask.on = true
			if hasValue {
				if value = strings.TrimSpace(value); value == "" {
					return nil, ask, usageErr("--remote-control= needs a name after the equals sign, or none at all")
				}
				ask.name = value
			}
			continue
		}
		out = append(out, a)
	}
	return out, ask, nil
}

// asksRemoteControl is holdwire.AsksRemoteControl for the CLI's own
// arguments on this machine, where --settings rota cannot read asks for
// nothing: Claude Code says what is wrong with it itself.
func asksRemoteControl(args []string, cwd string) bool {
	asks, _ := holdwire.AsksRemoteControl(args, cwd)
	return asks
}

// getwd is os.Getwd, and a variable only so a test can stand in for a
// directory that cannot be read, which macOS still names once removed.
var getwd = os.Getwd

// remoteInsideCwd is remoteOwnArgs's refusal where the current directory
// cannot be read.
const remoteInsideCwd = "Remote Control in the CLI's own arguments is refused when the current directory cannot be read, " +
	"as it is inside a held session; run it from a directory that exists, or ask for it with rota's own --remote-control, before --"

// remoteOwnArgs is the one exception to a claude account's CLI arguments,
// handed over on this machine, being the person's own. Remote Control asked
// for in them from inside a held session is refused (remoteInsideHeld).
// That covers a --settings that cannot be read, which might ask for it. A
// current directory that cannot be read is taken as inside: it is how an
// agent could step out of the check, and a relative --settings cannot be
// read without it. Anywhere else they are handed over untouched.
func remoteOwnArgs(s *store.Store, args []string) error {
	cwd, werr := getwd()
	asks, err := holdwire.AsksRemoteControl(args, cwd)
	switch {
	case !asks && err == nil:
		return nil
	case werr != nil:
		return errors.New(remoteInsideCwd)
	}
	return remoteInsideHeld(s)
}

// remoteInsideHeld refuses Remote Control asked for in the CLI's own
// arguments from inside a held session. The caller there is most likely the
// agent in one, which may have been started from another device. It is
// refused the way rota serve stop is, and rota's own --remote-control, which
// readies the account, still works there.
func remoteInsideHeld(s *store.Store) error {
	terminals, err := s.TerminalDir()
	if err == nil {
		err = refusedInsideHeldSession("Remote Control in the CLI's own arguments", terminals)
	}
	if err != nil {
		return fmt.Errorf("%w, or ask for it with rota's own --remote-control, before --", err)
	}
	return nil
}

// remoteControl readies an account for a session with Remote Control on, by
// the store's one rule for it (store.ReadyRemoteControl), and returns what
// to add to Claude Code's own arguments.
//
// The setting is turned on and saved first, because it is what gives the
// account the configuration file Remote Control reads its identity from —
// and once said, it stays said. Then the run is refused, with the reason,
// whenever Remote Control could not work in it: a session that starts with a
// Remote Control doomed to fail is worse than none.
func (c *cli) remoteControl(s *store.Store, a *rota.Account, ask remoteAsk) ([]string, error) {
	if !ask.on {
		return nil, nil
	}
	// A flag this account cannot take at all, which is a usage error, said
	// in the words it has always been said in.
	if rota.Flavor(a.Provider) != "claude" {
		return nil, usageErr("--remote-control is Claude Code's, and %s is a %s account", a, a.Provider)
	}
	turnedOn, err := s.ReadyRemoteControl(a)
	if turnedOn {
		fmt.Fprintf(c.err, "rota: remote control is on for %s from now on; `rota set %d --remote-control off` turns it off\n",
			nameOf(a), a.ID)
	}
	if err != nil {
		return nil, err
	}
	// Last, so nothing in the arguments before it can be read as its name.
	if ask.name != "" {
		return []string{"--remote-control", ask.name}, nil
	}
	return []string{"--remote-control"}, nil
}
