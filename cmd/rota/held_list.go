package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

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
// wrote is applied in one place, and it is the rule an account's name is
// printed by (see nameOf): a control character is a space, and so is a
// newline, so an escape in a session's name never reaches the terminal as a
// command and a name never ends its row or its sentence early. A JSON
// field carries the value as it is.
func heldCell(s string) string { return oneLine(s) }

// heldSessions is every held session the store's records in terminals
// describe, newest first, each as the daemon describes a session it knows
// only from its record — running, with its holder's pid, until it stops;
// then stopped, with how and when, and whether Resume can take it up — and
// in the same document, so --json prints what GET /v1/terminals lists. Its
// account's label comes from labels. A directory with no record in it — a
// holder on its way, or one that never got there — is no session yet, and
// is left out. So is a record that cannot be read, whose id is in unread.
func heldSessions(terminals string, labels map[int]string) (out []rotaclient.Session, unread []string) {
	for _, r := range holdwire.Scan(terminals) {
		if r.State == holdwire.StateBroken {
			if !errors.Is(r.Err, fs.ErrNotExist) {
				unread = append(unread, r.ID)
			}
			continue
		}
		// Strings, numbers and their maps, which encoding/json always
		// encodes, an odd byte in a folder's name included.
		raw, _ := json.Marshal(r.Describe(labels))
		var s rotaclient.Session
		if json.Unmarshal(raw, &s) == nil {
			out = append(out, s)
		}
	}
	slices.Sort(unread)
	return out, unread
}

// listHeld is the held sessions for `rota list --sessions`, labelled by the
// store's accounts, and the line that says which records it left out for
// being unreadable, "" when it left out none.
func listHeld(s *store.Store) (held []rotaclient.Session, note string) {
	terminals, err := s.TerminalDir()
	if err != nil {
		return nil, ""
	}
	labels := map[int]string{}
	for _, a := range s.Accounts {
		labels[a.ID] = a.Label()
	}
	held, unread := heldSessions(terminals, labels)
	switch len(unread) {
	case 0:
	case 1:
		note = "1 held session is not listed, since its record cannot be read: " + unread[0]
	default:
		note = fmt.Sprintf("%d held sessions are not listed, since their records cannot be read: %s",
			len(unread), strings.Join(unread, ", "))
	}
	return held, note
}

// showHeld prints the held sessions, when there are any: ● for one that is
// running and ○ for one that has stopped, which says how.
//
// width is the terminal's, or 0. Each row is one line, lined up by the
// columns its cells take on screen, so a name in Chinese or an emoji lines
// up too. On a terminal narrower than a row the free text is cut to fit, as
// the sessions above it are: the name keeps its start and the folder its
// end, each eight columns wide until the account has given way as far, then
// the account, the CLI and how the session is, and the start of its id,
// which rota send takes, never. Anywhere else nothing changes.
//
// note, when there is one, says which records were left out, on a line of
// its own under the rows, as the sessions' notes are said.
func (c *cli) showHeld(held []rotaclient.Session, note string, width int) {
	if len(held) == 0 && note == "" {
		return
	}
	fmt.Fprintln(c.out, "\nHeld sessions:")
	// The name shares its cell with the dot and the space before it (see
	// heldName), two more columns than the folder at every floor: the
	// wider of the two is cut first, and the folder then goes the last two
	// columns alone.
	g := grid{plain: true, width: width,
		cutBy: map[int]func(string, int) string{0: heldName, 1: cut, 4: cutStart},
		steps: []stage{{[]int{0, 4}, heldMark + cutSoft}, {[]int{4}, cutSoft}, {[]int{1}, cutSoft},
			{[]int{0, 4}, heldMark + cutHard}, {[]int{4}, cutHard}, {[]int{1}, cutHard}, {[]int{2}, cutHard}, {[]int{3}, cutHard}}}
	for _, h := range held {
		dot, what := "●", h.Status
		if h.Ended != nil {
			dot, what = "○", h.State
		}
		who := "-"
		if h.Account != 0 {
			who = fmt.Sprintf("#%d %s", h.Account, heldCell(h.AccountLabel))
		}
		g.add(dot+" "+clip(heldCell(h.Name), 44), who, orDash(heldCell(h.Provider)), heldCell(what),
			orDash(heldCell(h.Cwd)), heldCell(short(h.ID)))
	}
	g.render(c.out)
	if note != "" {
		sayIn(c.out, width, "note: "+note)
	}
}

// heldMark is the columns the dot and the space after it take before a
// held session's name.
const heldMark = 2

// heldName cuts the name in a held session's first cell to fit n columns
// with the dot before it, as cutCell cuts a title, keeping its start: the
// dot and its space stay, so a name whose first character is two columns
// wide is "○ …" rather than a dot against its "…".
func heldName(c string, n int) string {
	dot, name, _ := strings.Cut(c, " ")
	return dot + " " + cutCell(name, n-heldMark)
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
