package tui

import (
	"context"
	"slices"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/wire"
)

func init() { register(tabsAccounts, newAccounts) }

// accountKeys are the Accounts tab's keys. Every one but Up, Down and
// Refresh changes an account or runs one, and is gated: a watch principal
// is offered none of them, and the server would refuse them anyway.
type accountKeys struct {
	Up, Down, Run, MoveUp, MoveDown, More, Less, Typed, Out, First, Add, Relogin, Edit, Remove, Refresh key.Binding
}

func accountsKeys() accountKeys {
	return accountKeys{
		Up:       key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:     key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Run:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "run")),
		MoveUp:   key.NewBinding(key.WithKeys("shift+up", "K"), key.WithHelp("⇧↑/K", "move up")),
		MoveDown: key.NewBinding(key.WithKeys("shift+down", "J"), key.WithHelp("⇧↓/J", "move down")),
		More:     key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+/=", "max usage +5")),
		Less:     key.NewBinding(key.WithKeys("-"), key.WithHelp("-", "max usage -5")),
		Typed:    key.NewBinding(key.WithKeys("%"), key.WithHelp("%", "max usage")),
		Out:      key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "out of rotation or back")),
		First:    key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "use first")),
		Add:      key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "add")),
		Relogin:  key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "sign in again")),
		Edit:     key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		Remove:   key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove")),
		Refresh:  key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "read usage")),
	}
}

// accountsDoing is what each key reaches. The actions live in other
// tasks' files (accounts_act.go, edit.go, login.go, launch.go), which
// replace their stubs; a test puts recorders in a tab's own instead, to see
// which action a key reached whatever those files hold (F14).
type accountsDoing struct {
	launch    func(a *accountsTab, acct wire.Account) tea.Cmd
	move      func(a *accountsTab, acct wire.Account, place string) tea.Cmd
	nudge     func(a *accountsTab, acct wire.Account, delta int) tea.Cmd
	typed     func(a *accountsTab, acct wire.Account) overlay
	toggleOut func(a *accountsTab, acct wire.Account) tea.Cmd
	crossed   func(a *accountsTab, before, after []wire.Account) tea.Cmd
	login     func(e *env, again *wire.Account) asider
	edit      func(e *env, acct wire.Account) overlay
	remove    func(e *env, acct wire.Account) overlay
}

// accountsStep is how far +, = and - move a threshold (R36).
const accountsStep = 5

// accountsTab is the Accounts tab: every account in the rotation's order,
// with its usage against its own threshold, and the selected one's every
// window and setting below.
type accountsTab struct {
	e    *env
	keys accountKeys
	// rows is the latest listing, in its order; seen says one has come.
	rows []wire.Account
	seen bool
	// sel is the selected account's id, 0 for none. want is an id a
	// selectMsg asked for that no listing has held yet.
	sel, want int
	// top is the first row drawn, and shown how many rows the last View
	// drew, which a click is read against.
	top, shown int
	// failed is why the accounts could not be read, until a listing comes.
	failed error
	// do is what each key reaches, the other tasks' actions.
	do accountsDoing
	// act is what the account actions (accounts_act.go) keep between a
	// change sent and its answer being listed.
	act accountsActState
}

func newAccounts(e *env) tab {
	return &accountsTab{e: e, keys: accountsKeys(), do: accountsDoing{
		launch:    (*accountsTab).launch,
		move:      (*accountsTab).move,
		nudge:     (*accountsTab).nudge,
		typed:     (*accountsTab).typedThreshold,
		toggleOut: (*accountsTab).toggleOut,
		crossed:   (*accountsTab).crossed,
		login:     newLogin,
		edit:      newEdit,
		remove:    newRemove,
	}}
}

func (a *accountsTab) Title() string             { return "Accounts" }
func (a *accountsTab) Typing() bool              { return false }
func (a *accountsTab) Busy() string              { return "" }
func (a *accountsTab) Leave(ctx context.Context) {}

func (a *accountsTab) Update(msg tea.Msg) (tab, tea.Cmd) {
	switch msg := msg.(type) {
	case accountsActAnswer:
		return a, a.answered(msg)
	case listingMsg:
		if msg.Err == nil {
			return a, a.listed(msg.Listing.Accounts)
		}
		if !a.seen {
			a.failed = msg.Err
		}
	case selectMsg:
		a.pick(msg.Account)
	case tea.KeyPressMsg:
		return a, a.key(msg)
	case tea.MouseClickMsg:
		if m := msg.Mouse(); m.Button == tea.MouseLeft {
			a.click(m.Y)
		}
	case tea.MouseWheelMsg:
		switch msg.Mouse().Button {
		case tea.MouseWheelUp:
			a.step(-1)
		case tea.MouseWheelDown:
			a.step(1)
		}
	}
	return a, nil
}

// listed takes a new listing. The selection follows the account's id: an
// account asked for by selectMsg once it is listed, else the one selected
// wherever it moved, else the one now at its place when it went, else the
// first. Every listing after the first is checked for an account that went
// over its threshold (R11).
func (a *accountsTab) listed(rows []wire.Account) tea.Cmd {
	before, first := a.rows, !a.seen
	at := a.index()
	a.rows, a.seen, a.failed = slices.Clone(rows), true, nil
	switch {
	case a.want != 0 && a.has(a.want):
		a.sel, a.want = a.want, 0
	case a.has(a.sel):
	case len(a.rows) == 0:
		a.sel = 0
	case at < 0:
		a.sel = a.rows[0].ID
	default:
		a.sel = a.rows[min(at, len(a.rows)-1)].ID
	}
	a.act.listed()
	if first {
		return nil
	}
	return a.do.crossed(a, before, a.rows)
}

// pick selects id now when it is listed, and once it is when it is not yet:
// an account just added appears selected.
func (a *accountsTab) pick(id int) {
	if a.has(id) {
		a.sel, a.want = id, 0
		return
	}
	a.want = id
}

func (a *accountsTab) has(id int) bool {
	return id != 0 && slices.ContainsFunc(a.rows, func(r wire.Account) bool { return r.ID == id })
}

// index is the selected account's row, or -1.
func (a *accountsTab) index() int {
	if a.sel == 0 {
		return -1
	}
	return slices.IndexFunc(a.rows, func(r wire.Account) bool { return r.ID == a.sel })
}

func (a *accountsTab) selected() (wire.Account, bool) {
	if i := a.index(); i >= 0 {
		return a.rows[i], true
	}
	return wire.Account{}, false
}

// step moves the selection by d rows, staying in the table.
func (a *accountsTab) step(d int) {
	if len(a.rows) == 0 {
		return
	}
	i := min(max(a.index()+d, 0), len(a.rows)-1)
	a.sel, a.want = a.rows[i].ID, 0
}

// click selects the row drawn at line y of the tab, which is the header's
// at 0.
func (a *accountsTab) click(y int) {
	if r := y - 1; r >= 0 && r < a.shown && a.top+r < len(a.rows) {
		a.sel, a.want = a.rows[a.top+r].ID, 0
	}
}

// arm sets each key's state: gated for the principal, and those that act
// on the selected account off while none is.
func (a *accountsTab) arm() {
	k := &a.keys
	onAccount := []*key.Binding{&k.Run, &k.MoveUp, &k.MoveDown, &k.More, &k.Less, &k.Typed, &k.Out, &k.First, &k.Relogin, &k.Edit, &k.Remove}
	a.e.gate(append(onAccount, &k.Add)...)
	if _, ok := a.selected(); !ok {
		for _, b := range onAccount {
			b.SetEnabled(false)
		}
	}
}

// Keys are the keys the tab takes now, the actions first.
func (a *accountsTab) Keys() []key.Binding {
	a.arm()
	k := a.keys
	var out []key.Binding
	for _, b := range []key.Binding{k.Run, k.MoveUp, k.MoveDown, k.More, k.Less, k.Typed, k.Out, k.First, k.Add, k.Relogin, k.Edit, k.Remove, k.Refresh, k.Up, k.Down} {
		if b.Enabled() {
			out = append(out, b)
		}
	}
	return out
}

// key is what a key press does. An action that opens an overlay has the
// shell open it as this Update returns (env.open), so the keys typed
// after it reach it.
func (a *accountsTab) key(msg tea.KeyPressMsg) tea.Cmd {
	a.arm()
	k := &a.keys
	switch {
	case key.Matches(msg, k.Up):
		a.step(-1)
		return nil
	case key.Matches(msg, k.Down):
		a.step(1)
		return nil
	case key.Matches(msg, k.Refresh):
		return func() tea.Msg { return refreshMsg{How: RefreshForce} }
	case key.Matches(msg, k.Add):
		return a.e.open(a.do.login(a.e, nil))
	}
	acct, ok := a.selected()
	if !ok {
		return nil
	}
	switch {
	case key.Matches(msg, k.Run):
		return a.do.launch(a, acct)
	case key.Matches(msg, k.MoveUp):
		return a.do.move(a, acct, "up")
	case key.Matches(msg, k.MoveDown):
		return a.do.move(a, acct, "down")
	case key.Matches(msg, k.First):
		return a.do.move(a, acct, "first")
	case key.Matches(msg, k.More):
		return a.do.nudge(a, acct, accountsStep)
	case key.Matches(msg, k.Less):
		return a.do.nudge(a, acct, -accountsStep)
	case key.Matches(msg, k.Typed):
		return a.e.open(a.do.typed(a, acct))
	case key.Matches(msg, k.Out):
		return a.do.toggleOut(a, acct)
	case key.Matches(msg, k.Relogin):
		return a.e.open(a.do.login(a.e, &acct))
	case key.Matches(msg, k.Edit):
		return a.e.open(a.do.edit(a.e, acct))
	case key.Matches(msg, k.Remove):
		return a.e.open(a.do.remove(a.e, acct))
	}
	return nil
}

// accountsOpen asks the shell to open o, unless there is nothing to open,
// once its command's message comes back. env.open opens it as the Update
// returns, which a key's action wants: the keys typed after it reach it.
func accountsOpen(o overlay) tea.Cmd {
	if o == nil || o.Closed() {
		return nil
	}
	return func() tea.Msg { return openMsg{O: o} }
}
