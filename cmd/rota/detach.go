package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
	"github.com/professor93/rota/internal/termenv"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// rota run --detach: the account's CLI handed to a session the store's
// daemon holds, rather than to this terminal, and the command over at once.
// Nothing here prepares the account or runs the CLI. The daemon chooses
// what runs, as for every session it holds, and the holder it starts
// prepares it; this command says what to run, in which folder, at what
// size and with whose environment, and starts the daemon first when none
// runs.

// The refusals --detach answers with, before anything is started.
const (
	detachWithShare  = "--share and --detach are two different terminals; choose one"
	detachWithPrompt = "--detach hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --"
)

// detachCols and detachRows are the size a session starts at when standard
// input is no terminal to measure.
const (
	detachCols = 120
	detachRows = 40
)

// detach is `rota run [id] --detach`. The account named, or with none the
// rotation's choice, which is the daemon's to make; the CLI's arguments
// after -- or -i; and --label for the session's name.
//
// --remote-control with an account named is readied here, as a run that
// opens the CLI readies it, and its flag added to the arguments. With none
// named the account is not known until the daemon has chosen it, so the
// daemon is asked to ready whichever it chooses.
func (c *cli) detach(id int, rest []string, ask shareAsk, remote remoteAsk) error {
	if ask.on {
		return usageErr("%s", detachWithShare)
	}
	args, err := c.detachArgs(rest)
	if err != nil {
		return err
	}
	if !pty.Supported {
		return holdwire.ErrPlatform
	}
	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	req := rotaclient.Create{Account: id, Args: args, Label: strings.TrimSpace(ask.label)}
	if req.Env, err = absoluteDirs(store.HostEnv()); err != nil {
		return err
	}
	if req.Cwd, err = os.Getwd(); err != nil {
		return err
	}
	req.Cols, req.Rows = c.detachSize()
	var off map[int]bool
	switch {
	case remote.on && id != 0:
		more, err := c.remoteControlFor(id, remote)
		if err != nil {
			return err
		}
		req.Args = append(req.Args, more...)
	case remote.on:
		req.RemoteControl, req.RemoteControlName = true, remote.name
		off = c.remoteControlOff()
	}

	cl, started, err := rotaclient.Ensure(terminals, func() error {
		// serve start's own line is for a person who typed serve start;
		// this command says it in its own words, on stderr.
		quiet := *c
		quiet.out = io.Discard
		return quiet.startDaemon(true, "")
	})
	if started {
		fmt.Fprintf(c.err, "rota: started the rota daemon for %s (control socket only; rota serve stop ends it)\n", home)
	}
	if err != nil {
		return err
	}
	s, err := cl.Create(req)
	if err != nil {
		return err
	}
	if off[s.Account] {
		fmt.Fprintf(c.err, "rota: remote control is on for %s/%s from now on; `rota set %d --remote-control off` turns it off\n",
			s.Provider, s.AccountLabel, s.Account)
	}
	if c.json {
		return c.emit(s)
	}
	// The CLI's own name, as a run that opens it prints: the provider's
	// vocabulary, which for every provider rota has is its binary.
	bin := rota.Flavor(s.Provider)
	if bin == "" {
		bin = s.Provider
	}
	fmt.Fprintf(c.err, "rota: %s/%s via %s — held as session %s (%s); rota send %s \"...\" types into it, rota list --sessions lists it\n",
		s.Provider, s.AccountLabel, bin, s.Name, s.ID, shellWord(s.Name))
	return nil
}

// detachArgs is what --detach hands the CLI: what follows --, or what is
// left around -i, or nothing at all. Anything else is a prompt, which a
// session held for somebody to attach to later has nobody to answer; and
// -p or --print among the CLI's own arguments would have it answer one and
// exit. --json before -- is rota's own.
func (c *cli) detachArgs(rest []string) ([]string, error) {
	end := slices.Index(rest, "--")
	if end < 0 {
		end = len(rest)
	}
	own := slices.DeleteFunc(slices.Clone(rest[:end]), func(a string) bool {
		if a == "--json" {
			c.json = true
			return true
		}
		return false
	})
	rest = append(own, rest[end:]...)
	interactive := func(a string) bool { return a == "-i" || a == "--interactive" }
	var args []string
	switch i := slices.IndexFunc(rest, interactive); {
	case len(rest) > 0 && rest[0] == "--":
		args = rest[1:]
	case i >= 0:
		args = slices.Delete(rest, i, i+1)
	case len(rest) > 0:
		return nil, usageErr("%s", detachWithPrompt)
	}
	if slices.ContainsFunc(args, func(a string) bool { return a == "-p" || a == "--print" }) {
		return nil, usageErr("%s", detachWithPrompt)
	}
	return args, nil
}

// absoluteDirs is env with the Claude Code directories it names relatively
// named absolutely, as serve start names them for its daemon: a holder runs
// in /, where a relative one would be another directory.
func absoluteDirs(env []string) ([]string, error) {
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "ROTA_CLAUDE_HOME"} {
		for _, e := range env {
			if dir, ok := strings.CutPrefix(e, name+"="); ok && dir != "" && !filepath.IsAbs(dir) {
				abs, err := filepath.Abs(dir)
				if err != nil {
					return nil, err
				}
				env = termenv.Set(env, name, abs)
				break
			}
		}
	}
	return env, nil
}

// detachSize is the size of the terminal standard input is, which is the
// one the person typing this is looking at, or 120×40 when it is none.
func (c *cli) detachSize() (cols, rows uint16) {
	if f, ok := c.in.(*os.File); ok {
		if cols, rows, err := pty.Size(f); err == nil && cols > 0 && rows > 0 {
			return cols, rows
		}
	}
	return detachCols, detachRows
}

// remoteControlFor readies account id for Remote Control as a run that
// opens its CLI does, and says what to add to Claude Code's arguments.
func (c *cli) remoteControlFor(id int, remote remoteAsk) ([]string, error) {
	s, err := c.openStore()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	a, err := account(s, id)
	if err != nil {
		return nil, err
	}
	return c.remoteControl(s, a, remote)
}

// remoteControlOff is the claude accounts whose Remote Control setting is
// off now, so that once the daemon has chosen one and turned it on, this
// command can say so as a run that opens the CLI does. A store that cannot
// be read here says nothing; the daemon will say what is wrong with it.
func (c *cli) remoteControlOff() map[int]bool {
	s, err := c.openStore()
	if err != nil {
		return nil
	}
	defer s.Close()
	off := map[int]bool{}
	for _, a := range s.Accounts {
		if !a.RemoteControl && rota.Flavor(a.Provider) == "claude" {
			off[a.ID] = true
		}
	}
	return off
}

// shellWord is s as one word a shell reads back as s: as it is when nothing
// in it means anything to a shell, and in single quotes otherwise.
func shellWord(s string) string {
	if s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/@%+=:,", r))
	}) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
