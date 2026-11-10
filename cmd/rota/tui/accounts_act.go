package tui

// Task 13 fills this file in: moving an account in the rotation, its
// threshold, taking it out of the rotation and back, and the bell when an
// account crosses its threshold. Until then each action does nothing.

import (
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/wire"
)

// move puts acct at place in the rotation, as `rota set --order` takes it:
// up, down or first.
func (a *accountsTab) move(acct wire.Account, place string) tea.Cmd { return nil }

// nudge moves acct's threshold by delta, within 1..100 (R36).
func (a *accountsTab) nudge(acct wire.Account, delta int) tea.Cmd { return nil }

// typedThreshold is the overlay that takes acct's threshold as typed.
func (a *accountsTab) typedThreshold(acct wire.Account) overlay { return overlayClosed() }

// toggleOut takes acct out of the rotation, or puts it back last.
func (a *accountsTab) toggleOut(acct wire.Account) tea.Cmd { return nil }

// crossed rings the bell for each account that went over its threshold
// between two listings (R11).
func (a *accountsTab) crossed(before, after []wire.Account) tea.Cmd { return nil }
