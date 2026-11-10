package sshd

import (
	"context"
)

// tuiBare is the bare command: `ssh host` with nothing after it. On a
// terminal, with Options.TUI set, it is the TUI. Otherwise it is list's
// output, a blank line, and the commands, exit 0, or 1 when the list could
// not be had.
func tuiBare(ctx context.Context, ch *Channel) int {
	if ch.tui != nil && ch.PTY {
		return tuiOpen(ctx, ch)
	}
	code := listRun(ctx, ch, nil)
	dispatchText(ch.Out, ch.PTY, "\n"+usage)
	return code
}

// tuiOpen hands the channel to Options.TUI until it returns: exit 0, or 1
// with its error on stderr. What the person types reaches it through one
// reader of the channel (input), so the attaches it makes are typed into
// with nothing lost between them. Env holds TERM alone, and nothing when
// the client named no terminal rota takes.
func tuiOpen(ctx context.Context, ch *Channel) int {
	in := inputPump(ch.In)
	defer in.stop()
	c, stop := clientFor(ctx, ch)
	defer stop()
	var env []string
	if ch.Term != "" {
		env = []string{"TERM=" + ch.Term}
	}
	err := ch.tui(ctx, TUISession{
		In: in, Out: ch.Out, Env: env, Cols: ch.Cols, Rows: ch.Rows, Resize: ch.Resize,
		Client: c, Key: ch.Key, Taught: ch.Taught, Slot: ch.slot,
	})
	if err != nil {
		listFailed(ch, err)
		return 1
	}
	return 0
}
