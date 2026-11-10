package tui

// ⏎ on an account runs its CLI on this terminal, as `rota run <id>` does,
// and the TUI comes back when it ends (R28). With Options.Attach, and a
// daemon running or over SSH, it is to open a held session and attach to it
// instead (Task 19).

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"

	tea "charm.land/bubbletea/v2"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/wire"
)

// launch runs acct's CLI here. A dead account is not run: only a sign-in
// helps it.
func (a *accountsTab) launch(acct wire.Account) tea.Cmd {
	switch {
	case acct.Status == rota.StatusReauth:
		return toast(levelWarn, "account %d is dead: l signs it in again", acct.ID)
	case a.e.o.Attach != nil && (a.e.daemon.Running || a.e.o.Remote):
		return a.launchHeld(acct)
	}
	return a.launchHere(acct)
}

// launchHeld opens a held session on acct and attaches it to the terminal.
// Until Task 19 it runs the CLI here, as with no daemon.
func (a *accountsTab) launchHeld(acct wire.Account) tea.Cmd { return a.launchHere(acct) }

// launchHere hands the terminal to `rota run <id>`, made by Options.Exec,
// which is nil where this process's terminal is not the person's. It is a
// foreground run at this machine's own terminal, natural by default: rota
// adds no argument, setting or guard of its own to it. The command is made
// in the command launch returns, not in Update. Where there is no Exec the
// toast says so, whole within 80 columns, the toasts' room there.
func (a *accountsTab) launchHere(acct wire.Account) tea.Cmd {
	start := a.e.o.Exec
	if start == nil {
		return toast(levelWarn, "no terminal here to hand over; Sessions (a later beta) attaches over SSH")
	}
	id := acct.ID
	return func() tea.Msg {
		c := start("run", strconv.Itoa(id))
		if c == nil {
			return toastMsg{Text: fmt.Sprintf("rota run %d could not be started", id), Level: levelErr}
		}
		// execTail keeps what the CLI says last on stderr, which the
		// alternate screen hides once it comes back, and waits 2 s at most
		// for a process the CLI leaves holding that stderr.
		t := execTail(c)
		return handOffMsg{Cmd: t, Done: func(err error) tea.Msg { return launchEnded(id, t, err) }}
	}
}

// launchEnded is what the TUI does once `rota run <id>` gives the terminal
// back: it reads what is stale, since the run may have rotated, refreshed or
// spent an account, and says why a run that failed did, in the last line it
// wrote to stderr. The toast is cleaned where it is drawn.
func launchEnded(id int, t *tailed, err error) tea.Msg {
	refresh := func() tea.Msg { return refreshMsg{How: RefreshStale} }
	if err == nil {
		return refresh()
	}
	// Not an exit: it never started, or the terminal did not come back.
	text := fmt.Sprintf("rota run %d: %v", id, err)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			text = fmt.Sprintf("rota run %d exited %d", id, code)
		} else {
			text = fmt.Sprintf("rota run %d ended (%v)", id, exit)
		}
		if last := t.Last(); last != "" {
			text += ": " + last
		}
	}
	return tea.BatchMsg{refresh, toast(levelErr, "%s", text)}
}
