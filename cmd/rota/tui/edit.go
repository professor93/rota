package tui

// Task 14 fills this file in: editing every `rota set` flag of an account,
// removing one, and the confirm both use. Until then each overlay is closed
// as soon as it is opened.

import (
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/wire"
)

// newEdit is the form that edits acct.
func newEdit(e *env, acct wire.Account) overlay { return overlayClosed() }

// newRemove asks whether to forget acct, naming its live sessions.
func newRemove(e *env, acct wire.Account) overlay { return overlayClosed() }

// newConfirm asks title and body, and runs yes on a yes.
func newConfirm(e *env, title, body string, yes func() tea.Cmd) overlay { return overlayClosed() }
