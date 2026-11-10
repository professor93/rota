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
// commands that fill it are list, attach, watch and new, registered before
// any server starts; Listen takes its own copy.
var commands = map[string]command{}

// usage is what a bare command, or a wrong one, prints, with Ctrl-\ as the
// chord's key. dispatchUsage names the key a server was given.
const usage = `rota over ssh takes one of:
  list [--json]                      every session the daemon holds or lists
  attach <session>                   look and type; Ctrl-\ then d leaves
  watch <session>                    look only
  new [--account N] [--label NAME] [--cwd DIR] [--shell] [--json] [-- ARGS]
`

// dispatchUnknown is said, before usage, of a line that names no command.
const dispatchUnknown = "rota over ssh runs list, attach, watch and new, and nothing else\n"

// dispatchRun runs one command line on a channel and gives its exit status.
// bare is a shell request; an exec of an empty line is the same thing. The
// first word is looked up in cmds; one not there, or a line words cannot
// split, runs nothing and is exit 2. The log names the person and the word,
// never the arguments, and never a word that is not a command.
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
			dispatchText(ch.Err, ch.PTY, dispatchUnknown+dispatchUsage(ch.Key))
			return 2
		}
	}
	if len(w) == 0 {
		log.Info("ssh command", "name", ch.User.Name, "word", "")
		dispatchText(ch.Out, ch.PTY, dispatchUsage(ch.Key))
		return 0
	}
	run, ok := cmds[w[0]]
	if !ok {
		log.Info("ssh command", "name", ch.User.Name, "word", "", "refused", "unknown")
		dispatchText(ch.Err, ch.PTY, dispatchUnknown+dispatchUsage(ch.Key))
		return 2
	}
	log.Info("ssh command", "name", ch.User.Name, "word", w[0])
	return run(ctx, ch, w[1:])
}

// dispatchUsage is usage with the chord's key named as the server has it.
func dispatchUsage(key byte) string {
	return strings.Replace(usage, `Ctrl-\ then d`, dispatchKey(key)+" then d", 1)
}

// dispatchKey names a chord key as a person types it: Ctrl-\, Ctrl-],
// Ctrl-^, Ctrl-_, or Ctrl- and a letter. Zero is the default, Ctrl-\.
func dispatchKey(key byte) string {
	switch {
	case key == 0:
		return `Ctrl-\`
	case key >= 0x01 && key <= 0x1A:
		return "Ctrl-" + string(rune('A'+key-1))
	case key >= 0x1C && key <= 0x1F:
		return "Ctrl-" + string(rune('@'+key))
	}
	return fmt.Sprintf("the byte %#02x", key)
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
