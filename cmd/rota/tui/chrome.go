package tui

// The chrome is what the shell draws around the view with focus, and the
// keys and clicks that are its own (R7, R30): the header, with the tabs and
// whether a daemon runs; the footer, with the keys that work where the
// person is and the toasts above it; the help in place of the body, which
// takes every key and click while it is open; the two-step quit; and M,
// which turns the mouse on, so that a click on a tab in the header shows it.
//
// The shell (model.go) calls header, footer, helpView (chrome_help.go),
// chromeKey, chromeMouse and chromeUpdate, and keeps chromeState in
// Model.chrome.
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
// the oldest first; when a first quit press asked and what its ask said,
// zero while none waits; whether the help is open, with the first of its
// lines shown; and that the release of the click that closed it is still
// to come.
type chromeState struct {
	toasts  []chromeToast
	quitAt  time.Time
	ask     string
	help    bool
	top     int
	release bool
}

// chromeToast is a toast being shown: its text, cleaned, its level, and
// when it goes. quit marks the ask of the two-step quit: it is drawn as it
// reads when drawn, a second ask replaces it, and more toasts never push it
// out.
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

// chromeBindings are the keys the help takes while it is open, beyond the
// global ones.
type chromeBindings struct {
	Close, Up, Down key.Binding
}

func chromeKeys() chromeBindings {
	return chromeBindings{
		Close: key.NewBinding(key.WithKeys("esc", "?"), key.WithHelp("esc", "close")),
		// Up's help stands for both ways; Down has none of its own.
		Up:   key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/↓", "scroll")),
		Down: key.NewBinding(key.WithKeys("down", "j")),
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
	// A name is cut to no fewer than three columns and its "…".
	if rest := room - who("").width(); name != "" && rest >= 3+textWidth("…") {
		cut = who(strings.TrimRight(fit(name, rest), " "))
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
	return strings.Join(append(m.chromeToastLines(width), m.chromeKeysLine(width)), "\n")
}

// chromeToastLines are the toasts as drawn, the oldest first, each
// right-aligned one column in from the edge: the quit ask as it reads now,
// wrapped to the width, and every other toast cut with "…". The view of a
// terminal too small to draw shows them too (model.go), so a second quit
// press never ends something unseen.
func (m *Model) chromeToastLines(width int) []string {
	room := width - 2
	var out []string
	for _, t := range m.chrome.toasts {
		texts := []string{t.text}
		switch {
		case t.quit:
			texts = wrap(m.chromeAsk(), room)
		case textWidth(t.text) > room:
			texts = []string{strings.TrimRight(fit(t.text, room), " ")}
		}
		style := m.e.th.Good
		switch t.level {
		case levelWarn:
			style = m.e.th.Warn
		case levelErr:
			style = m.e.th.Over
		}
		for _, s := range texts {
			if s != "" {
				out = append(out, strings.Repeat(" ", max(width-1-textWidth(s), 0))+style.Render(s))
			}
		}
	}
	return out
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
		here, own = m.focusKeys(), []key.Binding{m.own.Interrupt}
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
		return " " + line(strings.TrimRight(fit(badge, rest), " "), nil).draw()
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
	case key.Matches(k, m.keys.Help) && !m.small():
		// Not while the terminal is too small to show it.
		m.chrome.help, m.chrome.top = true, 0
		return true, nil
	case key.Matches(k, m.keys.Mouse):
		return true, m.chromeMouseSwitch()
	}
	return false, nil
}

// chromeMouse takes every mouse message first. While the help is open it
// takes them all: the wheel scrolls it and a click closes it, the click's
// release too. Otherwise it takes the header's row, where a click on a
// tab's title shows that tab whenever no overlay is open, even while a
// field has focus, since a click is no text the field could take; and it
// leaves the rest to the shell.
func (m *Model) chromeMouse(msg tea.MouseMsg) (handled bool, cmd tea.Cmd) {
	if m.small() {
		return false, nil
	}
	m.chromeFollow()
	at := msg.Mouse()
	switch msg.(type) {
	case tea.MouseReleaseMsg:
		if m.chrome.release {
			m.chrome.release = false
			return true, nil
		}
	case tea.MouseClickMsg:
		m.chrome.release = false
	}
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
			m.chrome.help, m.chrome.release = false, true
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
// quits, when the ask still reads as it did then; otherwise it asks, so
// what has become busy since, or a key that no longer quits from where the
// person is, is said before anything ends.
func (m *Model) chromeQuit() tea.Cmd {
	now, ask := m.e.now(), m.chromeAsk()
	if !m.chrome.quitAt.IsZero() && now.Sub(m.chrome.quitAt) < chromeQuitWindow && ask == m.chrome.ask {
		m.chrome.quitAt, m.chrome.ask = time.Time{}, ""
		return tea.Quit
	}
	m.chrome.quitAt, m.chrome.ask = now, ask
	return m.chromeSay(ask, levelWarn, chromeQuitWindow, true)
}

// chromeAsk is the quit ask as it reads now: the key that quits from where
// the person is, and what quitting ends.
func (m *Model) chromeAsk() string {
	k := m.keys.Quit.Help().Key
	if m.typing() {
		k = m.own.Interrupt.Help().Key
	}
	ask := "press " + k + " again to quit"
	if ends := m.chromeEnds(); ends != "" {
		ask += "; " + ends
	}
	return ask
}

// chromeEnds says what quitting ends besides the TUI: what each tab is busy
// with, shown or not, as Busy names it. A login flow put aside is not among
// them: its login stays parked, and Run's caller says how to finish it (R18).
func (m *Model) chromeEnds() string {
	var what []string
	for _, t := range m.tabs {
		if b := clean(t.Busy()); b != "" {
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
