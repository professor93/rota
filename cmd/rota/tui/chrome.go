package tui

// The chrome is what the shell draws around the view with focus, and the
// keys and clicks that are its own (R7, R30): the header, with the tabs and
// whether a daemon runs; the footer, with the keys that work where the
// person is and the toasts above it; the help in place of the body, which
// takes every key and click while it is open; the two-step quit; and M,
// which turns the mouse on, so that a click on a tab in the header shows it.
//
// The shell (model.go) calls header, footer, helpView, chromeKey,
// chromeMouse and chromeUpdate, and keeps chromeState in Model.chrome.
// Nothing here does I/O, what is drawn depends on the model alone, and
// every string drawn passes through clean and is measured with textWidth.

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The chrome's times and limits (R30).
const (
	chromeQuitWindow = 2 * time.Second // a second quit press within it quits
	chromeToastFor   = 4 * time.Second // how long a toast is shown
	chromeToastsMax  = 3               // how many are shown at once, the newest
)

// chromeState is what the chrome keeps between messages: the toasts shown,
// the oldest first; when a first quit press asked, zero while none waits;
// and whether the help is open, with the first of its lines shown.
type chromeState struct {
	toasts []chromeToast
	quitAt time.Time
	help   bool
	top    int
}

// chromeToast is a toast being shown: its text, cleaned, its level, and
// when it goes. quit marks the ask of the two-step quit, which a second ask
// replaces and which more toasts never push out.
type chromeToast struct {
	text  string
	level int
	until time.Time
	quit  bool
}

// chromeFadeMsg is a toast's time being up: every toast whose time has
// come goes. A toast goes only on this message, never as the view is
// drawn, so the rows the footer takes do not change between Update and View.
type chromeFadeMsg struct{}

// chromeBindings are the chrome's keys beyond the global ones: those the
// help takes while it is open, and ctrl+c as the footer and the quit ask
// name it while a field takes q.
type chromeBindings struct {
	Close, Up, Down, Interrupt key.Binding
}

func chromeKeys() chromeBindings {
	return chromeBindings{
		Close: key.NewBinding(key.WithKeys("esc", "?"), key.WithHelp("esc", "close")),
		// Up's help stands for both ways; Down has none of its own.
		Up:        key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "scroll")),
		Down:      key.NewBinding(key.WithKeys("down", "j")),
		Interrupt: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	}
}

var chromeBound = chromeKeys()

// chromeSpan is a piece of a line the chrome draws: text that clean gave,
// and its style.
type chromeSpan struct {
	text  string
	style lipgloss.Style
}

// chromeLine is one line of the chrome, as spans.
type chromeLine []chromeSpan

func (l chromeLine) width() int {
	n := 0
	for _, s := range l {
		n += textWidth(s.text)
	}
	return n
}

func (l chromeLine) draw() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.style.Render(s.text))
	}
	return b.String()
}

// chromeCut is l in at most room columns: the span that crosses the edge
// cut with fit, the ones past it left out.
func chromeCut(l chromeLine, room int) chromeLine {
	var out chromeLine
	used := 0
	for _, s := range l {
		w := textWidth(s.text)
		if used+w <= room {
			out, used = append(out, s), used+w
			continue
		}
		if room > used {
			out = append(out, chromeSpan{fit(s.text, room-used), s.style})
		}
		break
	}
	return out
}

// chromeJoin is parts in one line, two spaces apart.
func (m *Model) chromeJoin(parts []chromeLine) chromeLine {
	var l chromeLine
	for i, p := range parts {
		if i > 0 {
			l = append(l, chromeSpan{"  ", m.e.th.Text})
		}
		l = append(l, p...)
	}
	return l
}

// header is one line: " rota", the tabs, and at the right end, one column
// in from the edge, whether a daemon runs and, over SSH, who is asking.
func (m *Model) header(width int) string {
	left, _ := m.chromeLeft()
	right := m.chromeRuns(width - left.width() - 3)
	if len(right) == 0 {
		return chromeCut(left, width).draw()
	}
	return left.draw() + strings.Repeat(" ", width-1-left.width()-right.width()) + right.draw()
}

// chromeLeft is the header's left part, and the columns each tab's title
// takes on it, from and to.
func (m *Model) chromeLeft() (chromeLine, [][2]int) {
	th := m.e.th
	l := chromeLine{{" ", th.Text}, {"rota", th.Title}}
	var at [][2]int
	for i, t := range m.tabs {
		gap := "  "
		if i == 0 {
			gap = "   "
		}
		style := th.Muted
		if i == m.active {
			// Underlined too, so the tab shown is told without colour.
			style = th.Accent.Underline(true)
		}
		title := clean(t.Title())
		l = append(l, chromeSpan{gap, th.Text})
		x := l.width()
		l = append(l, chromeSpan{title, style})
		at = append(at, [2]int{x, x + textWidth(title)})
	}
	return l, at
}

// chromeRuns is the header's right part, the fullest that fits in room:
// all of it; over SSH with the name cut; without who; without the count of
// sessions; nothing.
func (m *Model) chromeRuns(room int) chromeLine {
	th := m.e.th
	state := chromeLine{{"○ no daemon", th.Muted}}
	if d := m.e.daemon; d.Running {
		state = chromeLine{{"● daemon", th.Good}, {"  " + strconv.Itoa(len(d.Live)) + " live", th.Good}}
	}
	name, role := clean(m.e.who.Name), clean(m.e.who.Role)
	if !m.e.o.Remote || name+role == "" {
		return chromeFirst(room, state, state[:1])
	}
	sep := " · "
	if name == "" || role == "" {
		sep = ""
	}
	who := func(name string) chromeLine {
		return append(slices.Clone(state), chromeSpan{"  " + name + sep + role, th.Muted})
	}
	var cut chromeLine
	if rest := room - who("").width(); name != "" && rest >= 4 {
		cut = who(fit(name, rest))
	}
	return chromeFirst(room, who(name), cut, state, state[:1])
}

// chromeFirst is the first of lines that is not empty and fits in room, or
// none.
func chromeFirst(room int, lines ...chromeLine) chromeLine {
	for _, l := range lines {
		if len(l) > 0 && l.width() <= room {
			return l
		}
	}
	return nil
}

// footer is the toasts, the newest lowest, and under them the keys that
// work where the person is.
func (m *Model) footer(width int) string {
	var lines []string
	for _, t := range m.chrome.toasts {
		lines = append(lines, m.chromeToastLine(t, width))
	}
	return strings.Join(append(lines, m.chromeKeysLine(width)), "\n")
}

// chromeToastLine is a toast right-aligned one column in from the edge,
// cut with "…" where it is wider than that leaves.
func (m *Model) chromeToastLine(t chromeToast, width int) string {
	text := t.text
	if room := width - 2; textWidth(text) > room {
		text = fit(text, room)
	}
	style := m.e.th.Good
	switch t.level {
	case levelWarn:
		style = m.e.th.Warn
	case levelErr:
		style = m.e.th.Over
	}
	return strings.Repeat(" ", max(width-1-textWidth(text), 0)) + style.Render(text)
}

// chromeKeysLine is the footer's last line: the badge of a flow put aside,
// the keys of the view with focus, then the chrome's own. Where they do not
// fit, the view's keys give way from the last, then the badge is cut; the
// chrome's own always stay, since the help lists the rest.
func (m *Model) chromeKeysLine(width int) string {
	var here, own []key.Binding
	switch {
	case m.chromeHelpOpen():
		here, own = []key.Binding{chromeBound.Close}, []key.Binding{m.keys.Quit}
		if h := m.chromeBody(); len(m.chromeHelpLines(width, h)) > h {
			here = append(here, chromeBound.Up)
		}
	case m.typing():
		here, own = m.focusKeys(), []key.Binding{chromeBound.Interrupt}
	default:
		here, own = m.focusKeys(), []key.Binding{m.keys.Help, m.keys.Quit}
	}
	var badge string
	if m.aside != nil {
		badge = clean(m.aside.Badge(m.e.now()))
	}
	keys, tail := m.chromeShort(here), m.chromeShort(own)
	line := func(badge string, keys []chromeLine) chromeLine {
		var parts []chromeLine
		if badge != "" {
			parts = append(parts, chromeLine{{badge, m.e.th.Accent}})
		}
		return m.chromeJoin(append(append(parts, keys...), tail...))
	}
	room := width - 2
	for n := len(keys); n >= 0; n-- {
		if l := line(badge, keys[:n]); l.width() <= room {
			return " " + l.draw()
		}
	}
	if rest := room - line("", nil).width() - 2; badge != "" && rest > 0 {
		return " " + line(fit(badge, rest), nil).draw()
	}
	return " " + chromeCut(line("", nil), room).draw()
}

// chromeShort is each binding's short help, the key and what it does.
func (m *Model) chromeShort(bs []key.Binding) []chromeLine {
	var out []chromeLine
	for _, h := range chromeHelps(bs) {
		l := chromeLine{{h.Key, m.e.th.Key}}
		if h.Desc != "" {
			l = append(l, chromeSpan{" " + h.Desc, m.e.th.Muted})
		}
		out = append(out, l)
	}
	return out
}

// chromeHelps is the help of every binding that is enabled and has some,
// cleaned.
func chromeHelps(bs []key.Binding) []key.Help {
	var out []key.Help
	for _, b := range bs {
		if h := b.Help(); b.Enabled() && h.Key != "" {
			out = append(out, key.Help{Key: clean(h.Key), Desc: clean(h.Desc)})
		}
	}
	return out
}

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
// terminal's, less the header and the footer with its toasts (model.body,
// which the footer cannot call, since body asks the footer).
func (m *Model) chromeBody() int { return m.e.height - 2 - len(m.chrome.toasts) }

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

// chromeKey takes a key no field is typing, or ctrl+c from one. While the
// help is open it takes every key: ? or esc closes it, ↑ and ↓ scroll it,
// and q and M still do what they do everywhere.
func (m *Model) chromeKey(k tea.KeyPressMsg) (handled bool, cmd tea.Cmd) {
	m.chromeFollow()
	if m.chromeHelpOpen() {
		switch {
		case key.Matches(k, m.keys.Quit):
			return true, m.chromeQuit()
		case key.Matches(k, chromeBound.Close):
			m.chrome.help = false
		case key.Matches(k, m.keys.Mouse):
			return true, m.chromeMouseSwitch()
		case key.Matches(k, chromeBound.Up):
			m.chromeScroll(-1)
		case key.Matches(k, chromeBound.Down):
			m.chromeScroll(1)
		}
		return true, nil
	}
	switch {
	case key.Matches(k, m.keys.Quit):
		return true, m.chromeQuit()
	case key.Matches(k, m.keys.Help):
		m.chrome.help, m.chrome.top = true, 0
		return true, nil
	case key.Matches(k, m.keys.Mouse):
		return true, m.chromeMouseSwitch()
	}
	return false, nil
}

// chromeMouse takes every mouse message first. While the help is open it
// takes them all: the wheel scrolls it and a click closes it. Otherwise it
// takes the header's row, where a click on a tab's title shows that tab
// when no overlay is open, and leaves the rest to the shell.
func (m *Model) chromeMouse(msg tea.MouseMsg) (handled bool, cmd tea.Cmd) {
	if m.small() {
		return false, nil
	}
	m.chromeFollow()
	at := msg.Mouse()
	if m.chromeHelpOpen() {
		switch msg.(type) {
		case tea.MouseWheelMsg:
			switch at.Button {
			case tea.MouseWheelUp:
				m.chromeScroll(-1)
			case tea.MouseWheelDown:
				m.chromeScroll(1)
			}
		case tea.MouseClickMsg:
			m.chrome.help = false
		}
		return true, nil
	}
	if at.Y != 0 {
		return false, nil
	}
	if _, click := msg.(tea.MouseClickMsg); click && len(m.stack) == 0 {
		_, tabs := m.chromeLeft()
		for i, x := range tabs {
			if at.X >= x[0] && at.X < x[1] {
				m.active = i
			}
		}
	}
	return true, nil
}

// chromeUpdate takes every message that is not a key, the mouse or an
// effect: a toast to show, or a toast's time being up.
func (m *Model) chromeUpdate(msg tea.Msg) tea.Cmd {
	m.chromeFollow()
	switch msg := msg.(type) {
	case toastMsg:
		return m.chromeSay(msg.Text, msg.Level, chromeToastFor, false)
	case chromeFadeMsg:
		now := m.e.now()
		m.chrome.toasts = slices.DeleteFunc(m.chrome.toasts, func(t chromeToast) bool { return !t.until.After(now) })
	}
	return nil
}

// chromeFollow closes the help once a field has the keys (an overlay with
// its field focused opened over it), since only ctrl+c would reach it.
func (m *Model) chromeFollow() {
	if m.chrome.help && m.typing() {
		m.chrome.help = false
	}
}

// chromeSay shows text as a toast for d. A toast with the same text moves
// down rather than showing twice, a quit ask replaces the last one, and
// past three the oldest goes, never the quit ask. Its fade is a timer.
func (m *Model) chromeSay(text string, level int, d time.Duration, quit bool) tea.Cmd {
	text = clean(text)
	if text == "" {
		return nil
	}
	ts := slices.DeleteFunc(m.chrome.toasts, func(t chromeToast) bool { return t.text == text || (quit && t.quit) })
	ts = append(ts, chromeToast{text: text, level: level, until: m.e.now().Add(d), quit: quit})
	for len(ts) > chromeToastsMax {
		i := slices.IndexFunc(ts, func(t chromeToast) bool { return !t.quit })
		ts = slices.Delete(ts, i, i+1)
	}
	m.chrome.toasts = ts
	return m.e.tick(d, func(time.Time) tea.Msg { return chromeFadeMsg{} })
}

// chromeQuit is a quit press. One within chromeQuitWindow of the first
// quits; otherwise it asks, naming the key that works where the person is
// and what quitting would end.
func (m *Model) chromeQuit() tea.Cmd {
	now := m.e.now()
	if !m.chrome.quitAt.IsZero() && now.Sub(m.chrome.quitAt) < chromeQuitWindow {
		m.chrome.quitAt = time.Time{}
		return tea.Quit
	}
	m.chrome.quitAt = now
	k := m.keys.Quit.Help().Key
	if m.typing() {
		k = chromeBound.Interrupt.Help().Key
	}
	text := "press " + k + " again to quit"
	if ends := m.chromeEnds(); ends != "" {
		text += "; " + ends
	}
	return m.chromeSay(text, levelWarn, chromeQuitWindow, true)
}

// chromeEnds says what quitting ends besides the TUI: what each tab is
// busy with, shown or not, and the login flow put aside, by its badge.
func (m *Model) chromeEnds() string {
	var what []string
	for _, t := range m.tabs {
		if b := clean(t.Busy()); b != "" {
			what = append(what, b)
		}
	}
	if m.aside != nil {
		if b := clean(m.aside.Badge(m.e.now())); b != "" {
			what = append(what, b)
		}
	}
	switch n := len(what); n {
	case 0:
		return ""
	case 1:
		return what[0] + " ends too"
	default:
		return strings.Join(what[:n-1], ", ") + " and " + what[n-1] + " end too"
	}
}

// chromeMouseSwitch turns the mouse on or off, and says so: with it on the
// terminal no longer selects text (R7).
func (m *Model) chromeMouseSwitch() tea.Cmd {
	m.mouse = !m.mouse
	if m.mouse {
		return m.chromeSay("mouse on; M turns it off to select text", levelInfo, chromeToastFor, false)
	}
	return m.chromeSay("mouse off", levelInfo, chromeToastFor, false)
}
