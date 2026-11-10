package sshd

import (
	"context"
	"errors"
	"flag"
	"io"
	"slices"
	"strings"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/attach"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
)

// The refusals new answers with, exit 2, before anything is started.
const (
	newWithPrompt = "new hands the CLI its terminal, so it takes no prompt or -p: give the CLI's arguments after --\n"
	newShellAlone = "--shell runs a login shell, which takes no account or arguments\n"
)

// newCols and newRows are the size a session starts at when the channel
// has no pty to measure, as for rota run --detach with no terminal.
const (
	newCols = 120
	newRows = 40
)

// newRun is `new [--account N] [--label NAME] [--cwd DIR] [--shell]
// [--json] [-- ARGS]`: a session started for the person the key signed
// in, at the pty's size, or 120×40 with none. It runs with the daemon's
// environment, since only ctl.sock may hand a daemon another. The account
// left out is the rotation's choice, and --shell is the login shell, which
// the server's file must allow. On a terminal it then attaches to the
// session, as attach does; otherwise, or with --json, which asks for the
// session's document on a terminal too, it says the session started, or
// prints its document.
//
// What follows -- is the CLI's. Before it, a word that is not one of new's
// flags is a prompt, which a session started for somebody to attach to has
// nobody to answer; and -p or --print anywhere would have the CLI answer
// one and exit.
func newRun(ctx context.Context, ch *Channel, args []string) int {
	own, cli := args, []string(nil)
	if i := slices.Index(args, "--"); i >= 0 {
		own, cli = args[:i], args[i+1:]
	}
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	account := fs.Int("account", 0, "")
	label := fs.String("label", "", "")
	cwd := fs.String("cwd", "", "")
	shell := fs.Bool("shell", false, "")
	asJSON := fs.Bool("json", false, "")
	// Taken only to be refused in new's own words.
	p, printOne := fs.Bool("p", false, ""), fs.Bool("print", false, "")
	if err := fs.Parse(own); err != nil {
		say := usage
		if !errors.Is(err, flag.ErrHelp) {
			say = "new: " + attach.OneLine(err.Error()) + "\n" + usage
		}
		dispatchText(ch.Err, ch.PTY, say)
		return 2
	}
	if fs.NArg() > 0 || *p || *printOne || slices.ContainsFunc(cli, newPrints) {
		dispatchText(ch.Err, ch.PTY, newWithPrompt)
		return 2
	}
	named := false
	fs.Visit(func(f *flag.Flag) { named = named || f.Name == "account" })
	if *shell && (named || len(cli) > 0) {
		dispatchText(ch.Err, ch.PTY, newShellAlone)
		return 2
	}

	req := client.Create{Account: *account, Args: cli, Cwd: *cwd, Label: strings.TrimSpace(*label), Cols: newCols, Rows: newRows}
	if ch.PTY {
		// Held to 1–1000 by the channel.
		req.Cols, req.Rows = uint16(ch.Cols), uint16(ch.Rows)
	}
	if *shell {
		req.Kind = holdwire.KindShell
	}
	// On a terminal the session is attached to once it has started, and
	// the attach's place is taken first: with none to be had, nothing is
	// started that nobody could look at. A sign-in that may only watch
	// starts nothing, and takes no place to be told so by the API.
	attaching := ch.PTY && !*asJSON && ch.User.Role == api.RoleControl
	if attaching {
		done, ok := attachPlace(ch)
		if !ok {
			return 1
		}
		defer done()
	}
	// The create ends with the channel: a person who leaves, or a server
	// that stops, waits for no holder.
	c, stop := clientFor(ctx, ch)
	defer stop()
	s, err := c.Create(req)
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	if attaching {
		return attachTo(ctx, ch, c, s.ID, "attach")
	}
	if *asJSON {
		return listJSON(ch, s)
	}
	dispatchText(ch.Out, ch.PTY, "held as session "+attach.OneLine(listName(s))+" ("+attach.OneLine(s.ID)+")\n")
	return 0
}

// newPrints says a CLI argument asks the CLI to print one answer and exit:
// -p, --print or --print=…. A cluster of single letters is the CLI's to
// read, not new's: -dp is Claude Code's --debug with the filter p.
func newPrints(a string) bool {
	return a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=")
}
