package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// The smallest terminal the TUI draws in. Below it the screen says so, and
// nothing is forgotten (Review Focus 2).
const (
	modelMinWidth  = 60
	modelMinHeight = 14
)

// What the shell says to the tabs, and what they say to it. Every message
// but a key or the mouse reaches every tab and every overlay.
type (
	// listingMsg is a listing read with How, or why it could not be.
	listingMsg struct {
		Listing Listing
		Err     error
		How     Refresh
	}
	// daemonMsg is what Options.Daemon said.
	daemonMsg struct{ Daemon Daemon }
	// refreshMsg asks for the listing now, read as How says.
	refreshMsg struct{ How Refresh }
	// selectMsg asks the Accounts tab to select an account, by id.
	selectMsg struct{ Account int }
	// openMsg opens an overlay over the tabs. A nil or closed one is not
	// opened. A login flow opened while another is put aside brings that
	// one back instead (R17).
	openMsg struct{ O overlay }
	// toastMsg is a line said for a few seconds. Text is cleaned where it is
	// drawn.
	toastMsg struct {
		Text  string
		Level int
	}
)

// A toast's level.
const (
	levelInfo = iota
	levelWarn
	levelErr
)

// toast is the command that says a line for a few seconds.
func toast(level int, format string, a ...any) tea.Cmd {
	text := fmt.Sprintf(format, a...)
	return func() tea.Msg { return toastMsg{Text: text, Level: level} }
}

// modelWhoMsg is what Who answered.
type modelWhoMsg struct {
	Who Principal
	Err error
}

// modelPoll is one of the shell's own timers, and modelPollMsg its firing.
type modelPoll int

const (
	modelPollList   modelPoll = iota // the cached listing, every 15 s
	modelPollStale                   // a read of what is stale, every 2 min (R2)
	modelPollDaemon                  // whether a daemon runs, every 5 s (R30)
)

var modelPollEvery = [...]time.Duration{
	modelPollList:   15 * time.Second,
	modelPollStale:  2 * time.Minute,
	modelPollDaemon: 5 * time.Second,
}

type modelPollMsg struct{ Poll modelPoll }

// modelBindings are the shell's keys beyond the global ones.
type modelBindings struct {
	// Interrupt quits even from a field, where q types a q.
	Interrupt key.Binding
}

func modelKeys() modelBindings {
	return modelBindings{Interrupt: key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit"))}
}

// Model is the TUI: the tabs, the overlays over them and the chrome around
// them. Update does no I/O; everything it needs done is a command.
type Model struct {
	e      *env
	keys   globalKeys
	own    modelBindings
	tabs   []tab
	places []int
	active int
	// stack is the open overlays, the top one last. aside is the login
	// flow while it is put aside.
	stack []overlay
	aside asider
	mouse bool
	// chrome is the chrome's own state (chrome.go).
	chrome chromeState

	listing  [3]int // listing reads in flight, by Refresh
	whoKnown bool
	whoAsked bool
	daemonAt bool // a daemon call in flight
	// parkedWho, parkedList and parkedDaemon count the calls of each kind
	// that were answered for (env.callOr) and are still running. A timer
	// sends its kind again only at zero, so one that ignores its context
	// leaves at most one goroutine behind, while the failure is said at
	// once.
	parkedWho, parkedDaemon atomic.Int32
	parkedList              [3]atomic.Int32
}

// New makes the TUI's model for o, with every registered tab, and fills
// what o leaves nil.
func New(o Options) *Model {
	if o.In == nil {
		o.In = os.Stdin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Env == nil {
		o.Env = os.Environ()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Loc == nil {
		o.Loc = time.Local
	}
	if o.Tick == nil {
		o.Tick = tea.Tick
	}
	prof := Profile(o.Out, o.Env, o.Remote)
	if o.Profile != nil {
		prof = *o.Profile
	}
	e := &env{b: o.Backend, o: o, th: NewTheme(prof, true), ctx: context.Background(), win: &envWindow{}}
	if o.Cols > 0 && o.Rows > 0 {
		e.win.set(o.Cols, o.Rows)
	}
	m := &Model{e: e, keys: newGlobalKeys(), own: modelKeys()}
	m.tabs, m.places = tabsMade(e)
	return m
}

// Init asks who is asking, the listing as `rota list` reads it, whether a
// daemon runs and the terminal's background, and starts the timers that
// keep all of it fresh.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		m.askWho(),
		m.askAccounts(RefreshStale),
		tea.RequestBackgroundColor,
		m.pollIn(modelPollList),
		m.pollIn(modelPollStale),
	}
	if m.e.o.Daemon != nil {
		cmds = append(cmds, m.askDaemon(), m.pollIn(modelPollDaemon))
	}
	return tea.Batch(cmds...)
}

// Update routes msg: a key or the mouse to what has focus, every other
// message to every tab and overlay, after the shell has taken what it
// keeps of it.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Whatever gave a field focus, the help is not left open under it.
	defer m.chromeFollow()
	if cmd, ok := m.effect(msg); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return m, m.key(msg)
	case tea.MouseMsg:
		return m, m.mouseAt(msg)
	case tea.PasteMsg, tea.PasteStartMsg, tea.PasteEndMsg:
		// A paste is typed input: it goes where a key would, never to a
		// hidden tab or a flow put aside, nor under the open help.
		if m.small() || m.chromeHelpOpen() {
			return m, nil
		}
		return m, m.toFocus(msg)
	case openMsg:
		m.open(msg.O)
		return m, nil
	}
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.e.width, m.e.height = msg.Width, msg.Height
		if m.e.o.Resize == nil {
			m.e.win.set(msg.Width, msg.Height)
		} else if c, r := m.e.win.size(); c != msg.Width || r != msg.Height {
			// Run owns the size over SSH. One that differs is stale, as
			// Bubble Tea's own first size is when a resize was already
			// waiting: answer it with the latest, which converges.
			cmds = append(cmds, func() tea.Msg { return tea.WindowSizeMsg{Width: c, Height: r} })
		}
	case tea.BackgroundColorMsg:
		if msg.Color != nil {
			m.e.th = NewTheme(m.e.th.Profile, msg.IsDark())
		}
	case modelWhoMsg:
		m.whoAsked = false
		if msg.Err != nil {
			cmds = append(cmds, toast(levelErr, "the server did not say who you are: %v", msg.Err))
		} else {
			m.e.who, m.whoKnown = msg.Who, true
		}
	case listingMsg:
		if msg.How >= 0 && int(msg.How) < len(m.listing) && m.listing[msg.How] > 0 {
			m.listing[msg.How]--
		}
		if msg.Err != nil {
			cmds = append(cmds, toast(levelErr, "the accounts could not be read: %v", msg.Err))
		} else {
			m.e.list = msg.Listing
		}
	case daemonMsg:
		m.daemonAt = false
		m.e.daemon = msg.Daemon
	case refreshMsg:
		cmds = append(cmds, m.askAccounts(msg.How))
	case modelPollMsg:
		cmds = append(cmds, m.poll(msg.Poll))
	}
	cmds = append(cmds, m.chromeUpdate(msg), m.broadcast(msg))
	return m, tea.Batch(cmds...)
}

func (m *Model) small() bool {
	return m.e.width < modelMinWidth || m.e.height < modelMinHeight
}

// typing says the view with focus has a field focused.
func (m *Model) typing() bool {
	if n := len(m.stack); n > 0 {
		return m.stack[n-1].Typing()
	}
	if t := m.current(); t != nil {
		return t.Typing()
	}
	return false
}

func (m *Model) current() tab {
	if m.active < len(m.tabs) {
		return m.tabs[m.active]
	}
	return nil
}

// focusKeys are the bindings of the view with focus.
func (m *Model) focusKeys() []key.Binding {
	if n := len(m.stack); n > 0 {
		return m.stack[n-1].Keys()
	}
	if t := m.current(); t != nil {
		return t.Keys()
	}
	return nil
}

// key gives a key press to the chrome when no field has focus (ctrl+c
// always), then switches tabs on 1–4, [ and ] when nothing is open over
// them, and hands every other key to the view with focus.
func (m *Model) key(msg tea.KeyPressMsg) tea.Cmd {
	typing := m.typing()
	if !typing || key.Matches(msg, m.own.Interrupt) {
		if handled, cmd := m.chromeKey(msg); handled {
			return cmd
		}
	}
	if m.small() {
		return nil
	}
	if !typing && len(m.stack) == 0 {
		if place := tabsPlace(msg); place != 0 {
			for i, p := range m.places {
				if p == place {
					m.active = i
				}
			}
			return nil
		}
		if n := len(m.tabs); n > 0 {
			switch {
			case key.Matches(msg, m.keys.Next):
				m.active = (m.active + 1) % n
				return nil
			case key.Matches(msg, m.keys.Prev):
				m.active = (m.active + n - 1) % n
				return nil
			}
		}
	}
	return m.toFocus(msg)
}

// mouseAt gives a mouse message to the chrome, and one in the body to the
// view with focus, in the body's coordinates.
func (m *Model) mouseAt(msg tea.MouseMsg) tea.Cmd {
	if handled, cmd := m.chromeMouse(msg); handled {
		return cmd
	}
	if m.small() {
		return nil
	}
	top, height := m.body()
	at := msg.Mouse()
	at.Y -= top
	if at.Y < 0 || at.Y >= height {
		return nil
	}
	var moved tea.Msg
	switch msg.(type) {
	case tea.MouseClickMsg:
		moved = tea.MouseClickMsg(at)
	case tea.MouseReleaseMsg:
		moved = tea.MouseReleaseMsg(at)
	case tea.MouseWheelMsg:
		moved = tea.MouseWheelMsg(at)
	case tea.MouseMotionMsg:
		moved = tea.MouseMotionMsg(at)
	default:
		return nil
	}
	return m.toFocus(moved)
}

// body is the row the body starts on and how many rows it has, between
// the header and the footer.
func (m *Model) body() (top, height int) {
	top = modelLines(m.header(m.e.width))
	height = m.e.height - top - modelLines(m.footer(m.e.width))
	return top, max(height, 0)
}

func (m *Model) toFocus(msg tea.Msg) tea.Cmd {
	if n := len(m.stack); n > 0 {
		o, cmd := m.stack[n-1].Update(msg)
		m.stack[n-1] = o
		m.settle()
		return cmd
	}
	if t := m.current(); t != nil {
		nt, cmd := t.Update(msg)
		m.tabs[m.active] = nt
		return cmd
	}
	return nil
}

// broadcast hands msg to every tab, every open overlay and the flow put
// aside.
func (m *Model) broadcast(msg tea.Msg) tea.Cmd {
	var cmds []tea.Cmd
	for i, t := range m.tabs {
		nt, cmd := t.Update(msg)
		m.tabs[i] = nt
		cmds = append(cmds, cmd)
	}
	for i, o := range m.stack {
		no, cmd := o.Update(msg)
		m.stack[i] = no
		cmds = append(cmds, cmd)
	}
	if m.aside != nil {
		no, cmd := m.aside.Update(msg)
		cmds = append(cmds, cmd)
		if a, ok := no.(asider); ok {
			m.aside = a
		} else {
			m.aside = nil
			m.stack = append(m.stack, no)
		}
	}
	m.settle()
	return tea.Batch(cmds...)
}

// modelAbout is an overlay that says what it is about: its kind and the
// account it acts on. One that does not say is never taken for another.
type modelAbout interface {
	About() (kind string, account int)
}

// open puts o on top of the stack, unless the overlay on top is about the
// same: a key pressed twice before its overlay arrived (a double d) opens
// one. A login flow opened while one is put aside brings that one back and
// is dropped: one flow at a time (R17).
func (m *Model) open(o overlay) {
	if o == nil || o.Closed() {
		return
	}
	if n := len(m.stack); n > 0 {
		t, ok1 := m.stack[n-1].(modelAbout)
		u, ok2 := o.(modelAbout)
		if ok1 && ok2 {
			tk, ta := t.About()
			if uk, ua := u.About(); tk == uk && ta == ua {
				return
			}
		}
	}
	if _, ok := o.(asider); ok {
		if m.aside != nil {
			m.aside.Return()
			m.stack = append(m.stack, m.aside)
			m.aside = nil
			m.settle()
			return
		}
		for _, s := range m.stack {
			if _, open := s.(asider); open {
				return
			}
		}
	}
	m.stack = append(m.stack, o)
	m.settle()
}

// settle drops every closed overlay, takes a flow that says it is aside
// off the stack, and puts one that came back on top.
func (m *Model) settle() {
	var kept []overlay
	for _, o := range m.stack {
		if o.Closed() {
			continue
		}
		if a, ok := o.(asider); ok && a.Aside() {
			m.aside = a
			continue
		}
		kept = append(kept, o)
	}
	m.stack = kept
	if m.aside != nil && m.aside.Closed() {
		m.aside = nil
	}
	if m.aside != nil && !m.aside.Aside() {
		m.stack = append(m.stack, m.aside)
		m.aside = nil
	}
}

// result is what the TUI leaves behind: the parked login of a flow, aside
// or open.
func (m *Model) result() Result {
	if m.aside != nil {
		return Result{Pending: m.aside.Pending()}
	}
	for i := len(m.stack) - 1; i >= 0; i-- {
		if a, ok := m.stack[i].(asider); ok {
			return Result{Pending: a.Pending()}
		}
	}
	return Result{}
}

// View draws the header, the body and the footer, each cut to the
// terminal, or the size message when the terminal is too small.
func (m *Model) View() tea.View {
	v := tea.View{AltScreen: true}
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	w, h := m.e.width, m.e.height
	if m.small() {
		// The toasts still show, the quit ask among them.
		say := fmt.Sprintf("rota needs a terminal of at least %d×%d; this one is %d×%d", modelMinWidth, modelMinHeight, w, h)
		v.SetContent(strings.Join(append([]string{say}, m.chromeToastLines(w)...), "\n"))
		return v
	}
	head, foot := m.header(w), m.footer(w)
	_, height := m.body()
	body := m.helpView(w, height)
	if body == "" {
		switch t := m.current(); {
		case len(m.stack) > 0:
			body = m.stack[len(m.stack)-1].View(w, height)
		case t != nil:
			body = t.View(w, height)
		}
	}
	v.SetContent(strings.Join([]string{
		modelFrame(head, w, modelLines(head)),
		modelFrame(body, w, height),
		modelFrame(foot, w, modelLines(foot)),
	}, "\n"))
	return v
}

func modelLines(s string) int { return strings.Count(s, "\n") + 1 }

// modelFrame is s cut or padded to exactly h lines, each cut to w columns,
// so no part of the screen can push another.
func modelFrame(s string, w, h int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "")
	}
	return strings.Join(lines, "\n")
}

// The calls the shell makes. Each counts itself in flight, so a timer that
// fires while one is still out does not stack another.

func (m *Model) askWho() tea.Cmd {
	m.whoAsked = true
	b := m.e.b
	return m.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		p, err := b.Who(ctx)
		return modelWhoMsg{Who: p, Err: err}
	}, func(err error) tea.Msg { return modelWhoMsg{Err: err} }, &m.parkedWho)
}

// askAccounts reads the listing. A read that refreshes usage has
// envRefreshTimeout rather than an ordinary call's deadline.
func (m *Model) askAccounts(how Refresh) tea.Cmd {
	if how < RefreshNone || how > RefreshForce {
		how = RefreshNone
	}
	m.listing[how]++
	b, d := m.e.b, envCallTimeout
	if how != RefreshNone {
		d = envRefreshTimeout
	}
	return m.e.callOr(d, func(ctx context.Context) tea.Msg {
		l, err := b.Accounts(ctx, how)
		return listingMsg{Listing: l, Err: err, How: how}
	}, func(err error) tea.Msg { return listingMsg{Err: err, How: how} }, &m.parkedList[how])
}

// askDaemon asks Options.Daemon. One that panics or does not answer is a
// daemon that does not run, so the header never stays at a daemon gone.
func (m *Model) askDaemon() tea.Cmd {
	m.daemonAt = true
	ask := m.e.o.Daemon
	return m.e.callOr(envCallTimeout, func(ctx context.Context) tea.Msg {
		return daemonMsg{Daemon: ask(ctx)}
	}, func(err error) tea.Msg { return daemonMsg{Daemon: Daemon{Err: err.Error()}} }, &m.parkedDaemon)
}

func (m *Model) pollIn(p modelPoll) tea.Cmd {
	return m.e.tick(modelPollEvery[p], func(time.Time) tea.Msg { return modelPollMsg{Poll: p} })
}

// poll is a timer's firing: its call, unless one of its kind is still out
// or still running after it was answered for, and the timer set again.
func (m *Model) poll(p modelPoll) tea.Cmd {
	cmds := []tea.Cmd{m.pollIn(p)}
	switch p {
	case modelPollList:
		if !m.whoKnown && !m.whoAsked && m.parkedWho.Load() == 0 {
			cmds = append(cmds, m.askWho())
		}
		if m.listing[RefreshNone] == 0 && m.parkedList[RefreshNone].Load() == 0 {
			cmds = append(cmds, m.askAccounts(RefreshNone))
		}
	case modelPollStale:
		if m.listing[RefreshStale] == 0 && m.parkedList[RefreshStale].Load() == 0 {
			cmds = append(cmds, m.askAccounts(RefreshStale))
		}
	case modelPollDaemon:
		if m.e.o.Daemon != nil && !m.daemonAt && m.parkedDaemon.Load() == 0 {
			cmds = append(cmds, m.askDaemon())
		}
	}
	return tea.Batch(cmds...)
}
