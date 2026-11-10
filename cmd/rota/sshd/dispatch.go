package sshd

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// command is one thing a person may ask for over SSH. args are the words
// after the command's own; what it returns is the channel's exit-status.
type command func(ctx context.Context, ch *Channel, args []string) int

// commands is the fixed list a command line is looked up in, and nothing
// else is: no shell runs a line, and a word not here runs nothing. The
// commands that fill it are list, attach, watch and new, registered here,
// before any server starts; Listen takes its own copy.
var commands = map[string]command{
	"list": listRun,
	"new":  newRun,
}

// usage is what a bare command prints after the list, and a wrong one after
// what was wrong. The chord's key is Ctrl-\ in 1.4.0, with nothing to
// change it.
const usage = `rota over ssh takes one of:
  list [--json]                      every session the daemon holds or lists
  attach <session>                   look and type; Ctrl-\ then d leaves
  watch <session>                    look only
  new [--account N] [--label NAME] [--cwd DIR] [--shell] [--json] [-- ARGS]
`

// dispatchUnknown is said, before usage, of a line that names no command.
const dispatchUnknown = "rota over ssh runs list, attach, watch and new, and nothing else\n"

// dispatchRun runs one command line on a channel and gives its exit status.
// bare is a shell request; an exec of an empty line is the same thing, and
// both run the bare command (tuiBare). Otherwise the first word is looked
// up in cmds; one not there, or a line words cannot split, runs nothing and
// is exit 2. The log names the person and the word, never the arguments,
// and never a word that is not a command.
func dispatchRun(ctx context.Context, ch *Channel, line string, bare bool, cmds map[string]command, log *slog.Logger) int {
	if len(line) > maxLine {
		log.Info("ssh command", "name", ch.User.Name, "word", "", "refused", "too long")
		dispatchText(ch.Err, ch.PTY, fmt.Sprintf("rota over ssh takes a command line of at most %d bytes\n", maxLine))
		return 2
	}
	var w []string
	if !bare {
		var err error
		if w, err = words(line); err != nil {
			log.Info("ssh command", "name", ch.User.Name, "word", "", "refused", "unreadable")
			dispatchText(ch.Err, ch.PTY, dispatchUnknown+usage)
			return 2
		}
	}
	if len(w) == 0 {
		log.Info("ssh command", "name", ch.User.Name, "word", "")
		return tuiBare(ctx, ch)
	}
	run, ok := cmds[w[0]]
	if !ok {
		log.Info("ssh command", "name", ch.User.Name, "word", "", "refused", "unknown")
		dispatchText(ch.Err, ch.PTY, dispatchUnknown+usage)
		return 2
	}
	log.Info("ssh command", "name", ch.User.Name, "word", w[0])
	return run(ctx, ch, w[1:])
}

// dispatchText writes text a person reads. On a pty, the terminal at the
// other end is in raw mode and nobody turns a newline into a return and a
// newline for it, so this does.
func dispatchText(w io.Writer, pty bool, s string) {
	if pty {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	_, _ = io.WriteString(w, s)
}
