package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/professor93/rota/cmd/rota/attach"
	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/pty"
)

// rota run --attach <session>: this terminal on a session the store's
// daemon holds, by its name, label or id. What the session shows is drawn
// here, what is typed goes to it, and Ctrl-\ then d leaves it running; the
// rest of the chord is package attach's. Nothing is started: a session
// held by --detach, the web page or SSH is joined as it is, at its own size,
// and a window of another size is shown a crop until z fits it.

// The refusals --attach answers with before it looks for a daemon, each a
// usage error.
const (
	attachWithAccount  = "--attach takes a session, not an account: rota run --attach <session>"
	attachWithArgs     = "--attach takes no prompt or CLI arguments"
	attachWithOthers   = "--attach joins a session that is running; it takes no --detach, --share, --label or --remote-control"
	attachNoSession    = "--attach takes the session to show here: rota run --attach <session>"
	attachOffATerminal = `--attach needs a terminal; rota send <session> "text" types into one from a script`
)

// errAttachSignalled is what a lost attach says when a signal to this rota
// ended it, rather than the context's own words.
var errAttachSignalled = errors.New("rota was signalled to stop")

// attachTo is `rota run --attach <session>`. It takes no account, no
// prompt or arguments, and none of the flags that start a terminal; it
// needs this terminal, standard input and output alike, and a daemon that
// lists the session live. The attach then runs until the person leaves,
// the session ends, or it is lost; SIGTERM, SIGHUP or an interrupt sent to
// this rota ends it as lost. The terminal is given back as it was, and the
// one line saying why it ended goes to stderr. The exit code is the
// attach's: 0 for a leave, the CLI's own for an end, and 1 otherwise.
func (c *cli) attachTo(id int, rest []string, ask shareAsk, remote remoteAsk) error {
	switch {
	case id != 0:
		return usageErr("%s", attachWithAccount)
	case len(rest) > 0:
		return usageErr("%s", attachWithArgs)
	case ask.detach || ask.on || ask.label != "" || remote.on:
		return usageErr("%s", attachWithOthers)
	}
	if !pty.Supported {
		return holdwire.ErrPlatform
	}
	in, inOK := c.in.(*os.File)
	out, outOK := c.out.(*os.File)
	if !inOK || !outOK || !isTerminal(in) || !isTerminal(out) {
		return usageErr("%s", attachOffATerminal)
	}

	home, terminals, err := daemonStore()
	if err != nil {
		return err
	}
	cl, err := rotaclient.Dial(terminals)
	if errors.Is(err, rotaclient.ErrNoDaemon) {
		return c.sendFailed("no rota daemon is running for %s, so there is no session to attach to; rota run <id> --detach starts one", oneLine(home))
	}
	if err != nil {
		return c.sendFailed("%v", err)
	}
	s, err := cl.Resolve(ask.attach)
	if err != nil {
		return c.sendFailed("%v", err)
	}
	if s.State != "live" {
		name := s.Name
		if name == "" {
			name = s.ID
		}
		return c.sendFailed("session %s has ended; rota list --sessions shows it", heldCell(name))
	}

	ctx, stop := interrupted()
	defer stop()
	t, restore, err := attach.Local(in, out)
	if err != nil {
		return err
	}
	defer restore()
	res := attach.Run(ctx, t, cl, s.ID, attach.Options{
		Key:    attach.DaemonKey(terminals),
		Teach:  attach.TaughtFile(terminals),
		Redial: func() (*rotaclient.Client, error) { return rotaclient.Dial(terminals) },
	})
	// At once: a key typed before the terminal is handed back is dropped.
	restore()
	if res.Why == attach.Lost && ctx.Err() != nil {
		res.Err = errAttachSignalled
	}
	// The attach left the cursor on the window's last row, under the
	// session's own last line: the line goes below it.
	fmt.Fprint(out, "\r\n")
	fmt.Fprintln(c.err, res.Line(attach.Options{}))
	if code := res.ExitCode(); code != 0 {
		return exitCode(code)
	}
	return nil
}
