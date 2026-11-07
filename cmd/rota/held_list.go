package main

import (
	"errors"
	"fmt"
	"text/tabwriter"

	rotaclient "github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/holdwire"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// Held sessions as this command shows them without a daemon: from the
// store's records alone, which outlive both the daemon and the holders.

// heldCell is a value about a held session as this command prints it: a
// name, a label, a folder, read from a record or said by the daemon. Each
// one passes through here, so the rule for printing what somebody else
// wrote is applied in one place.
func heldCell(s string) string { return s }

// heldSessions is every held session the store's records in terminals
// describe, newest first, each as `rota run --detach --json` describes one,
// with its account's label from labels. A directory with no record in it —
// a holder on its way, or one that never got there — is no session yet,
// and is left out.
func heldSessions(terminals string, labels map[int]string) []rotaclient.Session {
	var out []rotaclient.Session
	for _, r := range holdwire.Scan(terminals) {
		if r.State != holdwire.StateBroken {
			out = append(out, heldSession(&r, labels))
		}
	}
	return out
}

// heldSession is one record as the daemon describes a session it knows
// only from its record: running, with its holder's pid, until it stops;
// then stopped, with how and when, and whether Resume can take it up.
func heldSession(r *holdwire.Record, labels map[int]string) rotaclient.Session {
	m := r.Meta
	s := rotaclient.Session{ID: r.ID, Name: r.Name(), Label: m.Name, Kind: m.Kind, Cwd: m.Cwd,
		Conversation: m.Conversation, Started: m.Started}
	if m.Kind != holdwire.KindShell && m.Account != 0 {
		s.Account, s.AccountLabel, s.Provider = m.Account, labels[m.Account], m.Provider
	}
	if r.Running() {
		s.State, s.Status, s.PID = string(holdwire.StateLive), "running", m.HolderPID
		s.Attachable = r.State == holdwire.StateLive
		return s
	}
	exit, ended := r.ExitCode(), r.Stopped
	s.State, s.Status, s.Exit, s.Ended = string(r.State), "stopped", &exit, &ended
	s.Resumable = r.Resumable()
	return s
}

// listHeld is the held sessions for `rota list --sessions`, labelled by the
// store's accounts.
func listHeld(s *store.Store) []rotaclient.Session {
	terminals, err := s.TerminalDir()
	if err != nil {
		return nil
	}
	labels := map[int]string{}
	for _, a := range s.Accounts {
		labels[a.ID] = a.Label()
	}
	return heldSessions(terminals, labels)
}

// showHeld prints the held sessions, when there are any: ● for one that is
// running and ○ for one that has stopped, which says how.
func (c *cli) showHeld(held []rotaclient.Session) {
	if len(held) == 0 {
		return
	}
	fmt.Fprintln(c.out, "\nHeld sessions:")
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	for _, h := range held {
		dot, what := "●", h.Status
		if h.Ended != nil {
			dot, what = "○", h.State
		}
		who := "-"
		if h.Account != 0 {
			who = fmt.Sprintf("#%d %s", h.Account, heldCell(h.AccountLabel))
		}
		fmt.Fprintf(w, "  %s %s\t%s\t%s\t%s\t%s\t%s\n", dot, clip(heldCell(h.Name), 44), who,
			orDash(heldCell(h.Provider)), heldCell(what), orDash(heldCell(h.Cwd)), heldCell(short(h.ID)))
	}
	_ = w.Flush()
}

// heldBusy is a busy account's refusal with the held session that has the
// account named at its end, when one does, in holdwire.HeldBy's words.
func heldBusy(s *store.Store, a *rota.Account, err error) error {
	if a == nil || !errors.Is(err, rota.ErrBusy) {
		return err
	}
	terminals, terr := s.TerminalDir()
	if terr != nil {
		return err
	}
	if by := holdwire.HeldBy(terminals, a.ID); by != "" {
		return fmt.Errorf("%w%s", err, heldCell(by))
	}
	return err
}
