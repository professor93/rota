package tui

// Task 15 fills this file in: the flow that adds an account on n and signs
// one in again on l. Until then the flow is closed as soon as it is opened.

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/wire"
)

// newLogin is the login flow: for a new account when again is nil, else
// for signing the account again names in again.
func newLogin(e *env, again *wire.Account) asider { return &loginStub{} }

// loginStub is the flow until Task 15: closed, so the shell drops it unseen.
type loginStub struct{}

func (l *loginStub) Update(tea.Msg) (overlay, tea.Cmd) { return l, nil }
func (*loginStub) View(int, int) string                { return "" }
func (*loginStub) Keys() []key.Binding                 { return nil }
func (*loginStub) Typing() bool                        { return false }
func (*loginStub) Closed() bool                        { return true }
func (*loginStub) Aside() bool                         { return false }
func (*loginStub) Badge(time.Time) string              { return "" }
func (*loginStub) Return()                             {}
func (*loginStub) Pending() *backendLogin              { return nil }
