package tui

import (
	"context"
	"fmt"
	"slices"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// tab is one screen of the TUI: Accounts, Sessions, Ask or Services. The
// shell gives it the keys while it is the one shown and no overlay is
// open, and every other message always, shown or not.
type tab interface {
	Title() string
	Update(msg tea.Msg) (tab, tea.Cmd)
	// View draws the tab in the space the chrome leaves it. A mouse
	// message reaches the tab in these coordinates too: (0, 0) is the
	// tab's own top left.
	View(width, height int) string
	// Keys are the tab's bindings as they stand, for the footer and the
	// help; a control-only one is passed through env.gate.
	Keys() []key.Binding
	// Typing says a field has focus: digits, [ ], q and M then go to it,
	// not to the shell.
	Typing() bool
	// Busy says what quitting would end, or "".
	Busy() string
	// Leave is called by Run after the program has ended: close whatever
	// is open, within ctx.
	Leave(ctx context.Context)
}

// The places tabs register at (R5). Key N goes to the tab at place N×10,
// so a key keeps its tab as tabs are added.
const (
	tabsAccounts = 10
	tabsSessions = 20
	tabsAsk      = 30
	tabsServices = 40
)

type tabsEntry struct {
	place int
	make  func(e *env) tab
}

// tabsRegistry is every tab registered, in the order registered.
var tabsRegistry []tabsEntry

// register adds the tab mk makes at place: Accounts 10, Sessions 20, Ask
// 30, Services 40. It is called from a tab file's init, so no shared file
// changes when a tab is added. Two tabs at one place is a mistake in the
// program, and panics.
func register(place int, mk func(e *env) tab) {
	for _, r := range tabsRegistry {
		if r.place == place {
			panic(fmt.Sprintf("tui: two tabs registered at place %d", place))
		}
	}
	tabsRegistry = append(tabsRegistry, tabsEntry{place: place, make: mk})
}

// tabsMade makes every registered tab for e, in the order of their places.
func tabsMade(e *env) (tabs []tab, places []int) {
	entries := slices.Clone(tabsRegistry)
	slices.SortFunc(entries, func(a, b tabsEntry) int { return a.place - b.place })
	for _, r := range entries {
		tabs = append(tabs, r.make(e))
		places = append(places, r.place)
	}
	return tabs, places
}

// tabsKeys are the keys 1 to 4, one binding each, so the shell can tell
// which was pressed without reading a key's name.
func tabsKeys() [4]key.Binding {
	var ks [4]key.Binding
	for i := range ks {
		n := string(rune('1' + i))
		ks[i] = key.NewBinding(key.WithKeys(n))
	}
	return ks
}

var tabsJump = tabsKeys()

// tabsPlace is the place a jump key goes to, or 0 when msg is none.
func tabsPlace(msg tea.KeyPressMsg) int {
	for i, b := range tabsJump {
		if key.Matches(msg, b) {
			return (i + 1) * 10
		}
	}
	return 0
}
