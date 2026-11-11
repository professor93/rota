package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

// chromeTab is a tab with bindings of its own, made fresh on each call as a
// tab makes them: two control-only ones passed through env.gate, one
// disabled and one with no help, so the footer and the help can be held to
// exactly the enabled ones. With many it has as many keys as the Accounts
// tab. It records the keys, pastes and clicks that reach it. A
// chromeFocusMsg focuses its field, and esc puts the field down.
type chromeTab struct {
	title  string
	e      *env
	many   bool
	busy   string
	typing bool
	lines  []string
	keys   []string
	pastes []string
	mice   []string
}

// chromeFocusMsg gives a chromeTab's field focus, as a message a tab hears
// after the chrome can.
type chromeFocusMsg struct{}

func (c *chromeTab) Title() string { return c.title }

func (c *chromeTab) Update(msg tea.Msg) (tab, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		c.keys = append(c.keys, msg.String())
		if msg.String() == "esc" {
			c.typing = false
		}
	case tea.PasteMsg:
		c.pastes = append(c.pastes, msg.Content)
	case chromeFocusMsg:
		c.typing = true
	case tea.MouseClickMsg:
		c.mice = append(c.mice, fmt.Sprintf("click %d,%d", msg.X, msg.Y))
	case tea.MouseReleaseMsg:
		c.mice = append(c.mice, fmt.Sprintf("release %d,%d", msg.X, msg.Y))
	case tea.MouseWheelMsg:
		c.mice = append(c.mice, fmt.Sprintf("wheel %d,%d", msg.X, msg.Y))
	}
	return c, nil
}

func (c *chromeTab) View(width, height int) string {
	if c.lines != nil {
		return strings.Join(c.lines, "\n")
	}
	return "tab " + c.title
}

func (c *chromeTab) Keys() []key.Binding       { return chromeTabKeys(c.e, c.many) }
func (c *chromeTab) Typing() bool              { return c.typing }
func (c *chromeTab) Busy() string              { return c.busy }
func (c *chromeTab) Leave(ctx context.Context) {}

func chromeTabKeys(e *env, many bool) []key.Binding {
	up := key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up"))
	add := key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "add"))
	remove := key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "remove"))
	refresh := key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	off := key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "never shown"), key.WithDisabled())
	bare := key.NewBinding(key.WithKeys("w"))
	e.gate(&add, &remove)
	bs := []key.Binding{up, add, remove, refresh, off, bare}
	if many {
		more := []key.Binding{
			key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "run")),
			key.NewBinding(key.WithKeys("shift+up", "K"), key.WithHelp("⇧↑/K", "move up")),
			key.NewBinding(key.WithKeys("shift+down", "J"), key.WithHelp("⇧↓/J", "move down")),
			key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+/-", "max % by 5")),
			key.NewBinding(key.WithKeys("%"), key.WithHelp("%", "type max %")),
			key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "out of rotation")),
			key.NewBinding(key.WithKeys("u"), key.WithHelp("u", "first")),
			key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "sign in again")),
			key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		}
		for i := range more {
			e.gate(&more[i])
		}
		bs = append(bs, more...)
	}
	return bs
}

// chromeOverlay is shellOverlay with bindings of its own, and a field that
// can have focus.
type chromeOverlay struct {
	shellOverlay
	bindings []key.Binding
	typing   bool
}

func (o *chromeOverlay) Update(msg tea.Msg) (overlay, tea.Cmd) {
	o.shellOverlay.Update(msg)
	return o, nil
}
func (o *chromeOverlay) Keys() []key.Binding { return o.bindings }
func (o *chromeOverlay) Typing() bool        { return o.typing }

// chromeAsider is a login flow put aside with a badge of the test's own.
type chromeAsider struct {
	*shellOverlay
	badge string
}

func (a chromeAsider) Update(msg tea.Msg) (overlay, tea.Cmd) {
	a.shellOverlay.Update(msg)
	return a, nil
}
func (a chromeAsider) Aside() bool            { return a.aside }
func (a chromeAsider) Badge(time.Time) string { return a.badge }
func (a chromeAsider) Return()                { a.aside = false }
func (a chromeAsider) Pending() *backendLogin { return nil }

// chromeRegister registers a chromeTab at each place, in a registry of the
// test's own, named as the TUI's tabs are.
func chromeRegister(t *testing.T, places ...int) map[int]*chromeTab {
	t.Helper()
	shellRegistry(t)
	names := map[int]string{10: "Accounts", 20: "Sessions", 30: "Ask", 40: "Services"}
	tabs := map[int]*chromeTab{}
	for _, p := range places {
		c := &chromeTab{title: names[p]}
		tabs[p] = c
		register(p, func(e *env) tab { c.e = e; return c })
	}
	return tabs
}

// chromeModel is a model over b on ticks, started at w×h.
func chromeModel(t *testing.T, b *fakeBackend, ticks *fakeTicks, w, h int, with ...func(*Options)) *Model {
	t.Helper()
	m := shellModel(t, b, ticks, with...)
	run(t, m, tea.BatchMsg{m.Init()}, size(w, h))
	return m
}

// chromeWho is a Backend whose Who answers name in role.
func chromeWho(name, role string) *fakeBackend {
	return &fakeBackend{WhoFn: func(context.Context) (Principal, error) {
		return Principal{Name: name, Role: role, Via: "ssh"}, nil
	}}
}

func chromeLines(m *Model) []string { return strings.Split(plain(m), "\n") }

// chromeRows is the body's rows, between the header and the footer with
// its toasts, each with its spaces run together, the blank ones at the end
// left out.
func chromeRows(m *Model) []string {
	lines := chromeLines(m)
	top, h := m.body()
	var out []string
	for _, l := range lines[top : top+h] {
		out = append(out, strings.Join(strings.Fields(l), " "))
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func chromeQuits(seen []tea.Msg) bool { return slices.Contains(seen, tea.Msg(tea.QuitMsg{})) }

// chromeAskRead is the quit ask as drawn, its rows joined: from the row it
// starts on to the footer's last line.
func chromeAskRead(m *Model) string {
	lines := chromeLines(m)
	var rows []string
	for i, l := range lines[:len(lines)-1] {
		if strings.HasPrefix(strings.TrimSpace(l), "press ") {
			for _, r := range lines[i : len(lines)-1] {
				rows = append(rows, strings.TrimSpace(r))
			}
			break
		}
	}
	return strings.Join(rows, " ")
}

func chromeTrueColor(o *Options) {
	p := colorprofile.TrueColor
	o.Profile = &p
}

func TestTheHeaderSaysWhatRuns(t *testing.T) {
	for _, c := range []struct {
		name   string
		daemon *Daemon
		remote bool
		right  string
		good   bool
	}{
		{"a daemon runs", &Daemon{Running: true, PID: 7, Live: []Live{{ID: "a"}, {ID: "b"}}}, false, "● daemon  2 live", true},
		{"none runs", nil, false, "○ no daemon", false},
		{"over SSH", &Daemon{Running: true}, true, "● daemon  0 live  alice · watch", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			chromeRegister(t, 10, 30)
			m := chromeModel(t, chromeWho("alice", "watch"), &fakeTicks{}, 80, 24, chromeTrueColor, func(o *Options) {
				o.Remote = c.remote
				if c.daemon != nil {
					d := *c.daemon
					o.Daemon = func(context.Context) Daemon { return d }
				}
			})
			head := chromeLines(m)[0]
			if !strings.HasPrefix(head, " rota   Accounts  Ask  ") {
				t.Errorf("the header is %q, want rota and the tabs at its left", head)
			}
			if !strings.HasSuffix(head, c.right) || textWidth(head) != 79 {
				t.Errorf("the header is %q, %d columns; want it to end with %q one column in from the edge", head, textWidth(head), c.right)
			}
			th := m.e.th
			state, style := "○ no daemon", th.Muted
			if c.good {
				state, style = "● daemon", th.Good
			}
			if !strings.Contains(m.View().Content, style.Render(state)) {
				t.Errorf("%q is not drawn in its style:\n%q", state, m.View().Content)
			}
			if !strings.Contains(m.View().Content, th.Accent.Underline(true).Render("Accounts")) || !strings.Contains(m.View().Content, th.Muted.Render("Ask")) {
				t.Errorf("the tab shown is not in Accent:\n%q", m.View().Content)
			}
			run(t, m, press("3")...)
			if !strings.Contains(m.View().Content, th.Accent.Underline(true).Render("Ask")) || !strings.Contains(m.View().Content, th.Muted.Render("Accounts")) {
				t.Errorf("after 3 the tab shown is not in Accent:\n%q", m.View().Content)
			}
		})
	}
}

func TestQuitTakesTwoPresses(t *testing.T) {
	for _, k := range []string{"q", "ctrl+c"} {
		t.Run(k, func(t *testing.T) {
			tabs := chromeRegister(t, 10, 30)
			ticks := &fakeTicks{}
			m := chromeModel(t, &fakeBackend{}, ticks, 80, 24)
			if _, seen := run(t, m, press(k)...); chromeQuits(seen) {
				t.Fatal("one press quit")
			}
			if !strings.Contains(plain(m), "press q again to quit") {
				t.Errorf("the first press does not ask:\n%s", plain(m))
			}
			run(t, m, ticks.advance(time.Second)...)
			if _, seen := run(t, m, press(k)...); !chromeQuits(seen) {
				t.Error("a second press within 2 s did not quit")
			}
			// Two seconds after a first press the ask is gone, and a press
			// asks again.
			run(t, m, press(k)...)
			run(t, m, ticks.advance(2*time.Second)...)
			if strings.Contains(plain(m), "again to quit") {
				t.Errorf("the ask outlived its two seconds:\n%s", plain(m))
			}
			if _, seen := run(t, m, press(k)...); chromeQuits(seen) {
				t.Error("a press two seconds after the first quit")
			}
			if !strings.Contains(plain(m), "press q again to quit") {
				t.Errorf("the press did not ask again:\n%s", plain(m))
			}
			if _, seen := run(t, m, press(k)...); !chromeQuits(seen) {
				t.Error("the press after asking again did not quit")
			}
			if len(tabs[10].keys)+len(tabs[30].keys) != 0 {
				t.Errorf("quit presses reached a tab: %q %q", tabs[10].keys, tabs[30].keys)
			}
		})
	}
	t.Run("q then ctrl+c", func(t *testing.T) {
		chromeRegister(t, 10)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
		if _, seen := run(t, m, press("q", "ctrl+c")...); !chromeQuits(seen) {
			t.Error("q and then ctrl+c did not quit")
		}
	})
	// In a field q is typed, so the ask names ctrl+c.
	t.Run("typing", func(t *testing.T) {
		tabs := chromeRegister(t, 10)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
		tabs[10].typing = true
		if _, seen := run(t, m, press("ctrl+c", "q")...); chromeQuits(seen) {
			t.Fatal("ctrl+c and a typed q quit")
		}
		if !strings.Contains(plain(m), "press ctrl+c again to quit") || !slices.Equal(tabs[10].keys, []string{"q"}) {
			t.Errorf("from a field the ask is wrong, or q did not reach the field (%q):\n%s", tabs[10].keys, plain(m))
		}
		if !strings.HasSuffix(chromeLines(m)[23], "ctrl+c quit") || strings.Contains(chromeLines(m)[23], "? help") {
			t.Errorf("from a field the footer is %q, want ctrl+c and not ?", chromeLines(m)[23])
		}
		if _, seen := run(t, m, press("ctrl+c")...); !chromeQuits(seen) {
			t.Error("a second ctrl+c from a field did not quit")
		}
	})
	// The ask names what quitting ends: a hidden tab's work and a flow put
	// aside.
	t.Run("busy", func(t *testing.T) {
		tabs := chromeRegister(t, 10, 30)
		ticks := &fakeTicks{}
		m := chromeModel(t, &fakeBackend{}, ticks, 120, 30)
		tabs[30].busy = "the open answer"
		run(t, m, press("q")...)
		if got, want := chromeAskRead(m), "press q again to quit; the open answer ends too"; got != want {
			t.Errorf("the ask is %q, want %q", got, want)
		}
		// Two seconds on, its fade not delivered yet, a press asks again,
		// and the new ask replaces the old. A login flow put aside is not
		// said to end: its login stays parked (R18).
		ticks.advance(2 * time.Second)
		tabs[10].busy = "the open run"
		run(t, m, openMsg{O: chromeAsider{&shellOverlay{asider: true}, "login abc123 · 9m"}}, press("esc")[0])
		run(t, m, press("q")...)
		if got, want := chromeAskRead(m), "press q again to quit; the open run and the open answer end too"; got != want {
			t.Errorf("the ask is %q, want %q", got, want)
		}
		if strings.Count(plain(m), "again to quit") != 1 {
			t.Errorf("a second ask did not replace the first:\n%s", plain(m))
		}
	})
	// Toasts that come after the ask never push it out.
	t.Run("kept", func(t *testing.T) {
		chromeRegister(t, 10)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
		run(t, m, press("q")[0], toastMsg{Text: "t1"}, toastMsg{Text: "t2"}, toastMsg{Text: "t3"})
		if p := plain(m); !strings.Contains(p, "press q again to quit") || strings.Contains(p, "t1") || !strings.Contains(p, "t3") {
			t.Errorf("three toasts after the ask:\n%s", p)
		}
	})
	// The ask is wrapped to the width, never cut.
	t.Run("wraps", func(t *testing.T) {
		tabs := chromeRegister(t, 10, 30)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 60, 20)
		tabs[10].busy, tabs[30].busy = "the open answer", "the open run"
		run(t, m, press("q")...)
		if got, want := chromeAskRead(m), "press q again to quit; the open answer and the open run end too"; got != want {
			t.Errorf("at 60 columns the ask is %q, want %q", got, want)
		}
		lines := chromeLines(m)
		for _, l := range lines[17:19] {
			if textWidth(l) != 59 || strings.Contains(l, "…") {
				t.Errorf("a row of the ask is %q, %d wide; want it whole, ending at column 59", l, textWidth(l))
			}
		}
	})
	// What has become busy since the first press is said before it ends.
	t.Run("busy since", func(t *testing.T) {
		tabs := chromeRegister(t, 10, 30)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
		run(t, m, press("q")...)
		tabs[30].busy = "the open answer"
		if got, want := chromeAskRead(m), "press q again to quit; the open answer ends too"; got != want {
			t.Errorf("the ask is drawn as %q, want %q", got, want)
		}
		if _, seen := run(t, m, press("q")...); chromeQuits(seen) {
			t.Error("a press quit what became busy after the ask")
		}
		if _, seen := run(t, m, press("q")...); !chromeQuits(seen) {
			t.Error("the press after the new ask did not quit")
		}
	})
	// Once a field has focus the ask names ctrl+c, and a ctrl+c asks again
	// rather than answer an ask that named q.
	t.Run("label", func(t *testing.T) {
		tabs := chromeRegister(t, 10)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
		run(t, m, press("q")...)
		tabs[10].typing = true
		if got := chromeAskRead(m); got != "press ctrl+c again to quit" {
			t.Errorf("with a field focused the ask is drawn as %q", got)
		}
		if _, seen := run(t, m, press("ctrl+c")...); chromeQuits(seen) {
			t.Error("ctrl+c quit on an ask that named q")
		}
		if _, seen := run(t, m, press("ctrl+c")...); !chromeQuits(seen) {
			t.Error("the second ctrl+c did not quit")
		}
	})
}

// M2: in a terminal too small to draw, the quit ask still shows under the
// size message, so a second press never ends something unseen; ? opens
// nothing there.
func TestChromeAsksToQuitWhenTooSmall(t *testing.T) {
	tabs := chromeRegister(t, 10)
	m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 50, 10)
	tabs[10].busy = "the open answer"
	if _, seen := run(t, m, press("q")...); chromeQuits(seen) {
		t.Fatal("one press quit")
	}
	lines := chromeLines(m)
	if lines[0] != "rota needs a terminal of at least 60×14; this one is 50×10" {
		t.Errorf("the first line is %q", lines[0])
	}
	var ask []string
	for _, l := range lines[1:] {
		if textWidth(l) > 49 {
			t.Errorf("a row of the ask is %d wide: %q", textWidth(l), l)
		}
		ask = append(ask, strings.TrimSpace(l))
	}
	if got, want := strings.Join(ask, " "), "press q again to quit; the open answer ends too"; got != want {
		t.Errorf("small, the ask is %q, want %q", got, want)
	}
	run(t, m, press("?")...)
	run(t, m, size(80, 24))
	if strings.Contains(plain(m), "Everywhere") {
		t.Errorf("? while small opened the help:\n%s", plain(m))
	}
	run(t, m, size(50, 10))
	if _, seen := run(t, m, press("q")...); !chromeQuits(seen) {
		t.Error("the second press after a seen ask did not quit")
	}
}

// M1: the help is modal for pastes too, and once a field has taken focus
// under it, it is shut and stays shut when the field is put down.
func TestChromeHelpTakesNoPasteAndStaysShut(t *testing.T) {
	tabs := chromeRegister(t, 10)
	m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
	run(t, m, press("?")[0], tea.PasteStartMsg{}, tea.PasteMsg{Content: "pasted"}, tea.PasteEndMsg{})
	if len(tabs[10].pastes) != 0 || !strings.Contains(plain(m), "Everywhere") {
		t.Errorf("a paste under the help reached the tab (%q), or closed it:\n%s", tabs[10].pastes, plain(m))
	}
	run(t, m, append([]tea.Msg{chromeFocusMsg{}}, press("x", "esc")...)...)
	if strings.Contains(plain(m), "Everywhere") || !slices.Equal(tabs[10].keys, []string{"x", "esc"}) {
		t.Errorf("after a field took focus and was put down the help is back, or the field took %q:\n%s", tabs[10].keys, plain(m))
	}
	run(t, m, press("?")...)
	o := &chromeOverlay{typing: true}
	run(t, m, append([]tea.Msg{openMsg{O: o}}, press("a", "b", "esc")...)...)
	if !o.closed || !slices.Equal(o.keys, []string{"a", "b", "esc"}) || strings.Contains(plain(m), "Everywhere") {
		t.Errorf("after a form with a field: closed %v, keys %q, help\n%s", o.closed, o.keys, plain(m))
	}
}

// Whatever reaches the chrome from outside, a name over SSH, a badge, a
// tab's title, a binding's help or what a tab is busy with, is drawn
// through clean: no escape, no key.
func TestChromeCleansWhatOthersSay(t *testing.T) {
	tabs := chromeRegister(t, 10, 30)
	secret := "sk-ant-api03-" + strings.Repeat("B", 40)
	tabs[30].title = "Ask\x1b[2J"
	m := chromeModel(t, chromeWho("eve\x1b[2J "+secret, "control"), &fakeTicks{}, 120, 30, func(o *Options) { o.Remote = true })
	run(t, m, openMsg{O: chromeAsider{&shellOverlay{asider: true}, "login \x1b]0;title\x07 " + secret}}, press("esc")[0])
	views := []string{m.View().Content}
	keys := []key.Binding{key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎\x1b[2J", "save "+secret))}
	run(t, m, openMsg{O: &chromeOverlay{bindings: keys}}, press("?")[0])
	views = append(views, m.View().Content)
	tabs[10].busy = "the answer\x1b[2J " + secret
	run(t, m, press("esc", "esc", "q")...)
	views = append(views, m.View().Content)
	for i, v := range views {
		if strings.Contains(v, "\x1b[2J") || strings.Contains(v, "\x1b]0;") || strings.Contains(v, secret) {
			t.Errorf("view %d draws what it was given as it was:\n%q", i, v)
		}
	}
	if p := plain(m); !strings.Contains(p, "eve sk-ant-[not repeated] · control") || !strings.Contains(p, " Ask ") ||
		strings.Contains(p, "[2J") || !strings.Contains(p, "the answer sk-ant-[not repeated] ends too") {
		t.Errorf("what was cleaned is not drawn:\n%s", p)
	}
}

// chromeHelpWant is what the help should list for a tab's bindings: its
// title and each enabled binding's help, then the global ones.
func chromeHelpWant(title string, here []key.Binding, global ...key.Binding) []string {
	want := []string{title}
	for _, b := range here {
		if b.Enabled() && b.Help().Key != "" {
			want = append(want, b.Help().Key+" "+b.Help().Desc)
		}
	}
	want = append(want, "", "Everywhere")
	for _, b := range global {
		want = append(want, b.Help().Key+" "+b.Help().Desc)
	}
	return want
}

func TestHelpListsExactlyTheEnabledKeys(t *testing.T) {
	g := newGlobalKeys()
	for _, role := range []string{"control", "watch"} {
		t.Run(role, func(t *testing.T) {
			tabs := chromeRegister(t, 10, 30)
			m := chromeModel(t, chromeWho("p", role), &fakeTicks{}, 80, 40)
			run(t, m, press("?")...)
			want := chromeHelpWant("Accounts", tabs[10].Keys(), g.Quit, g.Help, g.Mouse, g.Next, g.Prev, g.Jump)
			if got := chromeRows(m); !slices.Equal(got, want) {
				t.Errorf("the help lists\n%q\nwant\n%q", got, want)
			}
			if gated := slices.Contains(chromeRows(m), "n add"); gated != (role == "control") {
				t.Errorf("as %s the help lists n: %v", role, gated)
			}
		})
	}
	// With one tab there is nothing to switch to.
	t.Run("one tab", func(t *testing.T) {
		tabs := chromeRegister(t, 10)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 40)
		run(t, m, press("?")...)
		want := chromeHelpWant("Accounts", tabs[10].Keys(), g.Quit, g.Help, g.Mouse)
		if got := chromeRows(m); !slices.Equal(got, want) {
			t.Errorf("with one tab the help lists\n%q\nwant\n%q", got, want)
		}
	})
	// Over an overlay, its keys are listed, and switching tabs is not,
	// since it does not work there.
	t.Run("overlay", func(t *testing.T) {
		chromeRegister(t, 10, 30)
		m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 40)
		keys := []key.Binding{
			key.NewBinding(key.WithKeys("enter"), key.WithHelp("⏎", "save")),
			key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "hidden"), key.WithDisabled()),
		}
		run(t, m, openMsg{O: &chromeOverlay{bindings: keys}}, press("?")[0])
		want := chromeHelpWant("Here", keys, g.Quit, g.Help, g.Mouse)
		if got := chromeRows(m); !slices.Equal(got, want) {
			t.Errorf("over an overlay the help lists\n%q\nwant\n%q", got, want)
		}
	})
}

// While the help is open it takes every key and click: nothing reaches a
// tab, it scrolls when it does not fit, and a click or esc closes it.
func TestChromeHelpTakesTheInput(t *testing.T) {
	tabs := chromeRegister(t, 10, 30)
	tabs[10].many = true
	b := &fakeBackend{}
	m := chromeModel(t, b, &fakeTicks{}, 60, 14)
	assertNoIO(t, b, func() {
		m.Update(press("?")[0])
		m.View()
	})
	if !strings.Contains(plain(m), "Everywhere") || strings.Contains(plain(m), "tab Accounts") {
		t.Fatalf("? did not open the help:\n%s", plain(m))
	}
	foot := chromeLines(m)[13]
	if !strings.Contains(foot, "esc close") || !strings.Contains(foot, "↑/↓ scroll") || !strings.HasSuffix(foot, "q quit") || strings.Contains(foot, "refresh") {
		t.Errorf("with the help open the footer is %q, want its own keys", foot)
	}
	run(t, m, press("r", "3", "]", "enter")...)
	run(t, m, tea.MouseWheelMsg{X: 5, Y: 3, Button: tea.MouseWheelDown})
	if len(tabs[10].keys)+len(tabs[30].keys)+len(tabs[10].mice) != 0 || !strings.Contains(plain(m), "go to tab") {
		t.Errorf("with the help open a tab took %q %q %q", tabs[10].keys, tabs[30].keys, tabs[10].mice)
	}
	// The wheel moved it by a line; ↑ moves it back, and ↓ stops at its end.
	first := chromeRows(m)[0]
	run(t, m, press("up")...)
	if top := chromeRows(m)[0]; top == first || !strings.HasPrefix(top, "Accounts") {
		t.Errorf("the help did not scroll: %q then %q", first, top)
	}
	run(t, m, press(slices.Repeat([]string{"down"}, 30)...)...)
	if last := chromeRows(m); !strings.HasPrefix(last[len(last)-1], "e edit") {
		t.Errorf("scrolled to its end the help shows\n%q", last)
	}
	// Past its end it stops, so one ↑ moves it straight back.
	run(t, m, press("up")...)
	if last := chromeRows(m); strings.HasPrefix(last[len(last)-1], "e edit") {
		t.Errorf("one ↑ after scrolling past the end did not move the help:\n%q", last)
	}
	// With toasts above the footer it has fewer rows, and still scrolls to
	// its end and says it scrolls.
	run(t, m, toastMsg{Text: "one"}, toastMsg{Text: "two"})
	run(t, m, press(slices.Repeat([]string{"down"}, 30)...)...)
	if last := chromeRows(m); len(last) == 0 || !strings.HasPrefix(last[len(last)-1], "e edit") {
		t.Errorf("with two toasts, scrolled to its end the help shows\n%q", last)
	}
	if foot := chromeLines(m)[13]; !strings.Contains(foot, "↑/↓ scroll") {
		t.Errorf("with two toasts the footer is %q", foot)
	}
	// M keeps its meaning inside the help, which stays open.
	run(t, m, press("M")...)
	if m.View().MouseMode != tea.MouseModeCellMotion || strings.Contains(plain(m), "tab Accounts") {
		t.Errorf("M inside the help: mouse %v\n%s", m.View().MouseMode, plain(m))
	}
	// A click closes it, and neither it nor its release reaches the tab;
	// the next click and release are the tab's.
	run(t, m, tea.MouseClickMsg{X: 5, Y: 3, Button: tea.MouseLeft}, tea.MouseReleaseMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	if !strings.Contains(plain(m), "tab Accounts") || len(tabs[10].mice) != 0 {
		t.Errorf("a click did not close the help, or reached the tab (%q):\n%s", tabs[10].mice, plain(m))
	}
	run(t, m, tea.MouseClickMsg{X: 5, Y: 3, Button: tea.MouseLeft}, tea.MouseReleaseMsg{X: 5, Y: 3, Button: tea.MouseLeft})
	if want := []string{"click 5,2", "release 5,2"}; !slices.Equal(tabs[10].mice, want) {
		t.Errorf("after the help closed the tab took %q, want %q", tabs[10].mice, want)
	}
	// Opened again, it starts at its top.
	run(t, m, press("?")...)
	if rows := chromeRows(m); len(rows) == 0 || !strings.HasPrefix(rows[0], "Accounts") {
		t.Errorf("reopened, the help starts at %q", rows)
	}
	run(t, m, press("esc")...)
	for _, closer := range []string{"esc", "?"} {
		run(t, m, press("?", closer)...)
		if strings.Contains(plain(m), "Everywhere") {
			t.Errorf("%s did not close the help", closer)
		}
	}
	// q still asks to quit.
	if _, seen := run(t, m, press("?", "q")...); chromeQuits(seen) || !strings.Contains(plain(m), "press q again") {
		t.Errorf("q over the help: quit %v\n%s", chromeQuits(seen), plain(m))
	}
	// A field with focus has the keys: the help is not shown over it, and ?
	// is typed.
	// A field can take focus on a message the tabs hear after the chrome
	// has: the help is not drawn over it even before the next message.
	tabs[10].typing = true
	if strings.Contains(plain(m), "Everywhere") {
		t.Errorf("the help is shown over a field:\n%s", plain(m))
	}
	run(t, m, tea.BatchMsg{toast(levelInfo, "something")})
	run(t, m, press("?")...)
	tabs[10].typing = false
	if strings.Contains(plain(m), "Everywhere") || !slices.Equal(tabs[10].keys, []string{"?"}) {
		t.Errorf("? from a field opened the help or did not reach the field (%q)", tabs[10].keys)
	}
}

func TestToastsStackAndFade(t *testing.T) {
	chromeRegister(t, 10)
	ticks := &fakeTicks{}
	m := chromeModel(t, &fakeBackend{}, ticks, 80, 24, chromeTrueColor)
	run(t, m, toastMsg{Text: "toast-1"}, toastMsg{Text: "toast-2", Level: levelWarn}, toastMsg{Text: "toast-3", Level: levelErr}, toastMsg{Text: "toast-4"})
	th, v := m.e.th, m.View().Content
	if !strings.Contains(v, th.Warn.Render("toast-2")) || !strings.Contains(v, th.Over.Render("toast-3")) || !strings.Contains(v, th.Good.Render("toast-4")) {
		t.Errorf("a toast is not drawn in its level's style:\n%q", v)
	}
	lines := chromeLines(m)
	if len(lines) != 24 {
		t.Fatalf("the screen has %d lines", len(lines))
	}
	for i, want := range []string{"toast-2", "toast-3", "toast-4"} {
		line := lines[20+i]
		if strings.TrimLeft(line, " ") != want || textWidth(line) != 79 {
			t.Errorf("row %d is %q, want %q right-aligned one column in", 20+i, line, want)
		}
	}
	if strings.Contains(plain(m), "toast-1") {
		t.Errorf("a fourth toast did not push the first out:\n%s", plain(m))
	}
	// The same text again is moved down, not shown twice.
	run(t, m, toastMsg{Text: "toast-3", Level: levelErr})
	if strings.Count(plain(m), "toast-3") != 1 || strings.TrimSpace(chromeLines(m)[22]) != "toast-3" {
		t.Errorf("a repeated toast:\n%s", plain(m))
	}
	// Each lasts four seconds from when it came.
	run(t, m, ticks.advance(2*time.Second)...)
	run(t, m, toastMsg{Text: "late"})
	run(t, m, ticks.advance(2*time.Second)...)
	if p := plain(m); strings.Contains(p, "toast-") || !strings.Contains(p, "late") {
		t.Errorf("after 4 s:\n%s", p)
	}
	run(t, m, ticks.advance(2*time.Second)...)
	if strings.Contains(plain(m), "late") || strings.TrimSpace(chromeLines(m)[22]) != "" || len(chromeLines(m)) != 24 {
		t.Errorf("the last toast did not go, or the body did not take its row back:\n%s", plain(m))
	}
	// What a toast says is drawn through clean: no escape, no key.
	secret := "sk-ant-api03-" + strings.Repeat("A", 40)
	run(t, m, toastMsg{Text: "bad\x1b[2J\nnews " + secret, Level: levelErr})
	if v := m.View().Content; strings.Contains(v, "\x1b[2J") || strings.Contains(v, secret) || !strings.Contains(plain(m), "bad news sk-ant-[not repeated]") {
		t.Errorf("the toast was not cleaned:\n%q", v)
	}
}

func TestMouseIsOffUntilM(t *testing.T) {
	tabs := chromeRegister(t, 10, 30)
	m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 80, 24)
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("the mouse is %v before M", got)
	}
	run(t, m, press("M")...)
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion || !strings.Contains(plain(m), "mouse on") {
		t.Fatalf("after M the mouse is %v:\n%s", got, plain(m))
	}
	// A click on a tab's title in the header shows that tab.
	head := chromeLines(m)[0]
	ask, accounts := strings.Index(head, "Ask"), strings.Index(head, "Accounts")
	run(t, m, tea.MouseClickMsg{X: ask + 2, Y: 0, Button: tea.MouseLeft})
	if !strings.Contains(plain(m), "tab Ask") {
		t.Errorf("a click on Ask did not show it:\n%s", plain(m))
	}
	run(t, m, tea.MouseClickMsg{X: accounts, Y: 0, Button: tea.MouseLeft})
	if !strings.Contains(plain(m), "tab Accounts") {
		t.Errorf("a click on Accounts did not show it:\n%s", plain(m))
	}
	// Beside the tabs, and over an overlay, a click shows nothing new.
	run(t, m, tea.MouseClickMsg{X: 2, Y: 0, Button: tea.MouseLeft}, tea.MouseClickMsg{X: ask + 3, Y: 0, Button: tea.MouseLeft})
	o := &shellOverlay{}
	run(t, m, openMsg{O: o}, tea.MouseClickMsg{X: ask, Y: 0, Button: tea.MouseLeft}, press("esc")[0])
	if !strings.Contains(plain(m), "tab Accounts") || len(o.mice) != 0 || len(tabs[10].mice)+len(tabs[30].mice) != 0 {
		t.Errorf("a click beside the tabs or over an overlay reached %q %q %q:\n%s", o.mice, tabs[10].mice, tabs[30].mice, plain(m))
	}
	// A click in the body is still the tab's, in its own coordinates.
	run(t, m, tea.MouseClickMsg{X: 4, Y: 3, Button: tea.MouseLeft})
	if !slices.Equal(tabs[10].mice, []string{"click 4,2"}) {
		t.Errorf("a click in the body reached the tab as %q", tabs[10].mice)
	}
	run(t, m, press("M")...)
	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Errorf("a second M left the mouse %v", got)
	}
}

// Whatever it says, no line of the chrome is wider than the terminal: a
// long name over SSH is cut first, a long badge after the view's keys, a
// long toast with "…", and "? help  q quit" always stays.
func TestChromeFitsItsWidth(t *testing.T) {
	tabs := chromeRegister(t, 10, 20, 30, 40)
	tabs[10].many = true
	long := strings.Repeat("someone-with-a-long-name", 3)
	m := chromeModel(t, chromeWho(long, "control"), &fakeTicks{}, 80, 24, func(o *Options) {
		o.Remote = true
		o.Daemon = func(context.Context) Daemon { return Daemon{Running: true, Live: make([]Live, 12)} }
	})
	badge := "login " + strings.Repeat("waiting ", 12)
	run(t, m, openMsg{O: chromeAsider{&shellOverlay{asider: true}, badge}}, press("esc")[0],
		toastMsg{Text: strings.Repeat("a long toast ", 12)})
	for _, help := range []bool{false, true} {
		if help {
			run(t, m, press("?")...)
		}
		for w := 60; w <= 200; w++ {
			run(t, m, size(w, 24))
			lines := chromeLines(m)
			if len(lines) != 24 {
				t.Fatalf("at %d columns the screen has %d lines", w, len(lines))
			}
			for i, l := range lines {
				if textWidth(l) > w {
					t.Errorf("at %d columns row %d is %d wide: %q", w, i, textWidth(l), l)
				}
			}
			head, foot := lines[0], lines[23]
			if !strings.HasPrefix(head, " rota   Accounts  Sessions  Ask  Services  ") || !strings.Contains(head, "● daemon") || textWidth(head) != w-1 {
				t.Errorf("at %d columns the header is %q, %d wide", w, head, textWidth(head))
			}
			// The toast ends one column in from the edge, cut or not.
			if textWidth(lines[22]) != w-1 {
				t.Errorf("at %d columns the toast is %d wide: %q", w, textWidth(lines[22]), lines[22])
			}
			if w >= 120 && !strings.Contains(head, "12 live  someone") {
				t.Errorf("at %d columns the header leaves out who: %q", w, head)
			}
			if strings.Contains(head, "someone") && !strings.HasSuffix(head, "… · control") && !strings.HasSuffix(head, long+" · control") {
				t.Errorf("at %d columns the name is not cut whole: %q", w, head)
			}
			if textWidth(foot) > w-1 {
				t.Errorf("at %d columns the footer reaches the last column: %q", w, foot)
			}
			if _, name, ok := strings.Cut(head, "live  "); ok && strings.Contains(name, "…") {
				if kept, _, _ := strings.Cut(name, "…"); textWidth(kept) < 3 {
					t.Errorf("at %d columns the name is cut to %q", w, kept)
				}
			}
			if !strings.HasPrefix(foot, " login waiting") || !strings.HasSuffix(foot, "q quit") {
				t.Errorf("at %d columns the footer is %q", w, foot)
			}
			if !help && !strings.HasSuffix(foot, "? help  q quit") {
				t.Errorf("at %d columns the footer is %q", w, foot)
			}
			if toast := strings.TrimRight(lines[22], " "); !strings.HasSuffix(toast, "…") && !strings.HasSuffix(toast, "toast") {
				t.Errorf("at %d columns the toast is %q", w, toast)
			}
		}
	}
}

// A cut that ends on a wide character leaves no blank cell before what
// follows it: the name before " · role", the badge before the keys, a
// toast before the edge.
func TestChromeCutsEndOnALetter(t *testing.T) {
	chromeRegister(t, 10, 30)
	m := chromeModel(t, chromeWho(strings.Repeat("山田太郎", 10), "control"), &fakeTicks{}, 80, 24, func(o *Options) { o.Remote = true })
	run(t, m, openMsg{O: chromeAsider{&shellOverlay{asider: true}, "ログイン" + strings.Repeat("待ち", 40)}}, press("esc")[0],
		toastMsg{Text: strings.Repeat("日本語のアカウント", 10)})
	for w := 60; w <= 120; w++ {
		run(t, m, size(w, 24))
		lines := chromeLines(m)
		if head := lines[0]; !strings.HasSuffix(head, "… · control") {
			t.Errorf("at %d columns the header is %q", w, head)
		}
		if toast := lines[22]; strings.HasSuffix(toast, " ") || textWidth(toast) > w-1 {
			t.Errorf("at %d columns the toast is %q", w, toast)
		}
		if foot := lines[23]; strings.Contains(foot, "…   ") {
			t.Errorf("at %d columns the badge leaves a blank: %q", w, foot)
		}
	}
}

// TestChromeBothEastAsianModes runs the chrome's width tests again in the
// mode this run is not, as TestTextBothEastAsianModes does for the text
// measure: ●, ○, · and … are ambiguous, two columns where
// RUNEWIDTH_EASTASIAN=1, and every line must still fit. The golden is
// drawn narrow and stays out.
func TestChromeBothEastAsianModes(t *testing.T) {
	if os.Getenv(textModeVar) != "" {
		t.Skip("a child run")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := "wide"
	if textEastAsian() {
		mode = "narrow"
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "RUNEWIDTH_EASTASIAN=") && !strings.HasPrefix(kv, textModeVar+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, textModeVar+"="+mode)
	if mode == "wide" {
		env = append(env, "RUNEWIDTH_EASTASIAN=1")
	}
	cmd := exec.Command(exe, "-test.count=1", "-test.v", "-test.run",
		"^(TestTextModeIsTheOneAsked|TestTheHeaderSaysWhatRuns|TestToastsStackAndFade|TestChromeHelpTakesTheInput|TestChromeFitsItsWidth|TestChromeCutsEndOnALetter|TestQuitTakesTwoPresses)$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s run failed: %v\n%s", mode, err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestTextModeIsTheOneAsked") || !strings.Contains(string(out), "--- PASS: TestChromeFitsItsWidth") {
		t.Errorf("the %s run did not run in its mode:\n%s", mode, out)
	}
}

func TestTheChromeAt80x24(t *testing.T) {
	tabs := chromeRegister(t, 10, 30)
	tabs[10].many = true
	tabs[10].lines = []string{
		" #  account          5h      7d      reset",
		" 1  work@example     62%     21%     1h 3m (21:20)",
		" 2  home@example     12%      7%     4h 10m (00:27)",
	}
	m := chromeModel(t, chromeWho("alice", "control"), &fakeTicks{}, 80, 24, func(o *Options) {
		o.Remote = true
		o.Daemon = func(context.Context) Daemon {
			return Daemon{Running: true, PID: 7, Live: []Live{{ID: "a"}, {ID: "b"}}}
		}
	})
	run(t, m, openMsg{O: shellAsider{&shellOverlay{asider: true}}}, press("esc")[0],
		toastMsg{Text: "copied the link"},
		toastMsg{Text: "the accounts could not be read: the daemon went away", Level: levelErr})
	screen := plain(m)
	run(t, m, press("?")...)
	golden(t, "chrome-80x24", screen+"\n\n"+plain(m)+"\n")
}

// chromeBusyOverlay is an overlay busy with what its Busy names.
type chromeBusyOverlay struct {
	*shellOverlay
	busy string
}

func (o chromeBusyOverlay) Update(msg tea.Msg) (overlay, tea.Cmd) {
	o.shellOverlay.Update(msg)
	return o, nil
}
func (o chromeBusyOverlay) Busy() string { return o.busy }

// chromeBusyAsider is a flow put aside whose step keeps something running.
type chromeBusyAsider struct {
	chromeAsider
	busy string
}

func (a chromeBusyAsider) Update(msg tea.Msg) (overlay, tea.Cmd) {
	a.shellOverlay.Update(msg)
	return a, nil
}
func (a chromeBusyAsider) Busy() string { return a.busy }

// The quit ask names what open overlays keep running, the top one first,
// then the flow put aside, then the tabs; an overlay with nothing running,
// or with no Busy at all, adds nothing.
func TestTheQuitAskNamesWhatOverlaysKeepRunning(t *testing.T) {
	tabs := chromeRegister(t, 10)
	m := chromeModel(t, &fakeBackend{}, &fakeTicks{}, 160, 30)
	tabs[10].busy = "the open run"
	run(t, m, openMsg{O: chromeBusyAsider{chromeAsider{&shellOverlay{asider: true}, "login · aside"}, "the aside page"}}, press("esc")[0])
	if m.aside == nil {
		t.Fatal("the flow is not aside")
	}
	run(t, m, openMsg{O: chromeBusyOverlay{&shellOverlay{}, "the lower page"}},
		openMsg{O: chromeBusyOverlay{&shellOverlay{}, ""}},
		openMsg{O: &shellOverlay{}},
		openMsg{O: chromeBusyOverlay{&shellOverlay{}, "the top page"}})
	run(t, m, press("q")...)
	if got, want := chromeAskRead(m), "press q again to quit; the top page, the lower page, the aside page and the open run end too"; got != want {
		t.Errorf("the ask is %q, want %q", got, want)
	}
}
