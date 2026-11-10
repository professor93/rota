package tui

// This file is the stub of the chrome. Task 10 replaces all of it, keeping
// these signatures, which the shell (model.go) calls:
//
//	header(width) and footer(width)  the lines above and below the body
//	helpView(width, height)          the help in place of the body, or "" when it is closed
//	chromeKey(k)                     a key no field is typing; handled says the shell stops there
//	chromeMouse(msg)                 a mouse message, before the shell hands one in the body on
//	chromeUpdate(msg)                every message that is not a key, the mouse or an effect
//	chromeState                      the chrome's own fields on Model
//
// The stub draws one header line and one footer line, quits at once on q
// or ctrl+c, and toggles the mouse on M.

import (
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// chromeState is what the chrome keeps between messages: here only the
// last toast.
type chromeState struct {
	toast toastMsg
}

func (m *Model) header(width int) string {
	th := m.e.th
	parts := []string{th.Title.Render(" rota")}
	for i, t := range m.tabs {
		style := th.Muted
		if i == m.active {
			style = th.Accent
		}
		parts = append(parts, style.Render(t.Title()))
	}
	if m.e.daemon.Running {
		parts = append(parts, th.Good.Render("● daemon"))
	} else {
		parts = append(parts, th.Muted.Render("○ no daemon"))
	}
	return strings.Join(parts, "  ")
}

func (m *Model) footer(width int) string {
	th := m.e.th
	var parts []string
	if m.aside != nil {
		parts = append(parts, chromeSafe(m.aside.Badge(m.e.now())))
	}
	if t := m.chrome.toast.Text; t != "" {
		parts = append(parts, chromeSafe(t))
	}
	parts = append(parts, th.Key.Render("q")+" quit")
	return " " + strings.Join(parts, "  ")
}

func (m *Model) helpView(width, height int) string { return "" }

func (m *Model) chromeKey(k tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	switch {
	case key.Matches(k, m.keys.Quit):
		return true, tea.Quit
	case key.Matches(k, m.keys.Mouse):
		m.mouse = !m.mouse
		return true, nil
	}
	return false, nil
}

func (m *Model) chromeMouse(msg tea.MouseMsg) (handled bool, cmd tea.Cmd) { return false, nil }

func (m *Model) chromeUpdate(msg tea.Msg) tea.Cmd {
	if t, ok := msg.(toastMsg); ok {
		m.chrome.toast = t
	}
	return nil
}

// chromeSafe keeps only printable text on one line; Task 10 draws through
// clean instead.
func chromeSafe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return ' '
		case r < 0x20 || (r >= 0x7f && r < 0xa0):
			return -1
		}
		return r
	}, s)
}
