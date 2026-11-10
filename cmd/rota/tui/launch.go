package tui

// Task 18 fills this file in: ⏎ runs the account's CLI on this terminal,
// or in a held session. Until then it does nothing.

import (
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/wire"
)

// launch runs acct's CLI here.
func (a *accountsTab) launch(acct wire.Account) tea.Cmd { return nil }
