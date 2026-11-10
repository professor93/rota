package tui

// The help: every enabled binding of the view with focus and the global
// ones that work there, by group, drawn in place of the body (chrome.go
// takes the keys and clicks while it is open).

import (
	"strings"

	"charm.land/bubbles/v2/key"
)

// chromeGroup is one group of the help: its title and its bindings' help.
type chromeGroup struct {
	title string
	keys  []key.Help
}

// chromeHelp is what the help lists: the enabled bindings of the view with
// focus, under the tab's title or, for an overlay, "Here"; then the global
// ones that work there, tab switching only where tabs switch.
func (m *Model) chromeHelp() []chromeGroup {
	var groups []chromeGroup
	title := "Here"
	if t := m.current(); len(m.stack) == 0 && t != nil {
		title = clean(t.Title())
	}
	if here := chromeHelps(m.focusKeys()); len(here) > 0 {
		groups = append(groups, chromeGroup{title, here})
	}
	global := []key.Binding{m.keys.Quit, m.keys.Help, m.keys.Mouse}
	if len(m.stack) == 0 && len(m.tabs) > 1 {
		global = append(global, m.keys.Next, m.keys.Prev, m.keys.Jump)
	}
	return append(groups, chromeGroup{"Everywhere", chromeHelps(global)})
}

// chromeHelpLines lays the help out for a body of width×height: the groups
// one under another where that fits, else side by side where that does,
// else one under another to scroll. Each line is two columns in and at most
// width wide.
func (m *Model) chromeHelpLines(width, height int) []chromeLine {
	th := m.e.th
	var blocks [][]chromeLine
	for _, g := range m.chromeHelp() {
		kw := 0
		for _, h := range g.keys {
			kw = max(kw, textWidth(h.Key))
		}
		block := []chromeLine{{{g.title, th.Title}}}
		for _, h := range g.keys {
			block = append(block, chromeLine{{fit(h.Key, kw), th.Key}, {"  " + h.Desc, th.Text}})
		}
		blocks = append(blocks, block)
	}
	var lines []chromeLine
	for i, b := range blocks {
		if i > 0 {
			lines = append(lines, nil)
		}
		lines = append(lines, b...)
	}
	if side := m.chromeSide(blocks); len(lines) > height && len(side) < len(lines) && chromeWidest(side)+2 <= width {
		lines = side
	}
	out := make([]chromeLine, len(lines))
	for i, l := range lines {
		if len(l) > 0 {
			out[i] = append(chromeLine{{"  ", th.Text}}, chromeCut(l, width-2)...)
		}
	}
	return out
}

// chromeSide is blocks side by side, four columns apart, with no spaces
// left at the end of a row.
func (m *Model) chromeSide(blocks [][]chromeLine) []chromeLine {
	rows := 0
	for _, b := range blocks {
		rows = max(rows, len(b))
	}
	out := make([]chromeLine, rows)
	for r := range out {
		at, from := 0, 0 // where the row has got to, and where block i starts
		for i, b := range blocks {
			if i > 0 {
				from += chromeWidest(blocks[i-1]) + 4
			}
			if r >= len(b) {
				continue
			}
			if from > at {
				out[r] = append(out[r], chromeSpan{strings.Repeat(" ", from-at), m.e.th.Text})
			}
			out[r] = append(out[r], b[r]...)
			at = from + b[r].width()
		}
	}
	return out
}

func chromeWidest(lines []chromeLine) int {
	w := 0
	for _, l := range lines {
		w = max(w, l.width())
	}
	return w
}

// chromeHelpOpen says the help is shown: open, with no field taking keys,
// since the shell gives a field every key but ctrl+c.
func (m *Model) chromeHelpOpen() bool { return m.chrome.help && !m.typing() }

// chromeBody is the body's height as the chrome lays the screen out: the
// terminal's, less the header and the footer with its toasts' rows
// (model.body, which the footer cannot call, since body asks the footer).
func (m *Model) chromeBody() int { return m.e.height - 2 - len(m.chromeToastLines(m.e.width)) }

// helpView is the help in place of the body, or "" while it is closed.
func (m *Model) helpView(width, height int) string {
	if !m.chromeHelpOpen() || height <= 0 {
		return ""
	}
	lines := m.chromeHelpLines(width, height)
	top := max(0, min(m.chrome.top, len(lines)-height))
	var out []string
	for _, l := range lines[top:min(top+height, len(lines))] {
		out = append(out, l.draw())
	}
	if s := strings.Join(out, "\n"); s != "" {
		return s
	}
	return " "
}

// chromeScroll moves the help by n lines, within what it has.
func (m *Model) chromeScroll(n int) {
	h := m.chromeBody()
	last := len(m.chromeHelpLines(m.e.width, h)) - h
	m.chrome.top = max(0, min(m.chrome.top+n, last))
}
