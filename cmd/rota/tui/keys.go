package tui

import "charm.land/bubbles/v2/key"

// globalKeys are the keys every screen answers to. A tab's own keys come
// from the tab, and the help draws both from these bindings, never from a
// list of its own.
type globalKeys struct {
	Quit, Help, Mouse, Next, Prev, Jump key.Binding
}

func newGlobalKeys() globalKeys {
	return globalKeys{
		Quit:  key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
		Help:  key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Mouse: key.NewBinding(key.WithKeys("M"), key.WithHelp("M", "mouse")),
		Next:  key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "next tab")),
		Prev:  key.NewBinding(key.WithKeys("["), key.WithHelp("[", "previous tab")),
		Jump:  key.NewBinding(key.WithKeys("1", "2", "3", "4"), key.WithHelp("1-4", "go to tab")),
	}
}
