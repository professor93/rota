package main

import (
	"errors"
	"fmt"
	"path/filepath"
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

// heldName is what a session is called by its record alone, as the daemon
// calls one it describes from its record: what the person called it, the
// name its CLI gave the conversation, its folder, and its id.
func heldName(r *holdwire.Record) string {
	switch m := r.Meta; {
	case m == nil:
	case m.Name != "":
		return m.Name
	case m.ConversationName != "":
		return m.ConversationName
	case m.Cwd != "":
		return filepath.Base(m.Cwd)
	}
	return r.ID
}

// heldSessions is every held session the store's records describe, newest
// first, each as `rota run --detach --json` describes one. A directory with
// no record in it — a holder on its way, or one that never got there — is
// no session yet, and is left out.
func heldSessions(s *store.Store) []rotaclient.Session {
	terminals, err := s.TerminalDir()
	if err != nil {
		return nil
	}
	labels := map[int]string{}
	for _, a := range s.Accounts {
		labels[a.ID] = a.Label()
	}
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
// then stopped, with how and when, and resumable when it was interrupted
// on a conversation that is known.
func heldSession(r *holdwire.Record, labels map[int]string) rotaclient.Session {
	m := r.Meta
	s := rotaclient.Session{ID: r.ID, Name: heldName(r), Label: m.Name, Kind: m.Kind, Cwd: m.Cwd,
		Conversation: m.Conversation, Started: m.Started}
	if m.Kind != holdwire.KindShell && m.Account != 0 {
		s.Account, s.AccountLabel, s.Provider = m.Account, labels[m.Account], m.Provider
	}
	switch r.State {
	case holdwire.StateLive, holdwire.StateUnreadable:
		s.State, s.Status, s.PID = "live", "running", m.HolderPID
		s.Attachable = r.State == holdwire.StateLive
	default:
		exit, ended := -1, r.Stopped
		if m.Exit != nil {
			exit = *m.Exit
		}
		s.State, s.Status, s.Exit, s.Ended = string(r.State), "stopped", &exit, &ended
		s.Resumable = r.State == holdwire.StateInterrupted && m.Conversation != ""
	}
	return s
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
// account named at its end, when one does.
func heldBusy(s *store.Store, a *rota.Account, err error) error {
	if a == nil || !errors.Is(err, rota.ErrBusy) {
		return err
	}
	terminals, terr := s.TerminalDir()
	if terr != nil {
		return err
	}
	r := holdwire.Holding(terminals, a.ID)
	if r == nil {
		return err
	}
	return fmt.Errorf("%w — held by session %s (%s)", err, heldCell(heldName(r)), r.ID)
}
