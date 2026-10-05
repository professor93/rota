package main

import (
	"fmt"
	"strings"

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

// remoteControl readies an account for a session with Remote Control on, and
// returns what to add to Claude Code's own arguments.
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
	if rota.Flavor(a.Provider) != "claude" {
		return nil, usageErr("--remote-control is Claude Code's, and %s is a %s account", a, a.Provider)
	}
	if !a.RemoteControl {
		a.RemoteControl = true
		if err := s.Save(); err != nil {
			return nil, err
		}
		fmt.Fprintf(c.err, "rota: remote control is on for %s from now on; `rota set %d --remote-control off` turns it off\n",
			a, a.ID)
	}
	if !rota.StoresLogin(a, s.Home(a)) {
		return nil, fmt.Errorf("%w: Remote Control needs the account's own stored login, and %s does not run on one "+
			"(a dead login, a login without a refresh token, or Windows); Claude Code refuses it for a token in its environment",
			rota.ErrUnsupported, a)
	}
	if why := s.RemoteControlWaits(a); why != "" {
		return nil, fmt.Errorf("%w: %s; until then Remote Control would act as the wrong account, so this session is not started",
			rota.ErrBusy, why)
	}
	// Last, so nothing in the arguments before it can be read as its name.
	if ask.name != "" {
		return []string{"--remote-control", ask.name}, nil
	}
	return []string{"--remote-control"}, nil
}
