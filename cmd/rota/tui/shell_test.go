package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/professor93/rota/internal/fakecli"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// fakeBackend is the Backend every TUI test drives: each call is recorded,
// and answered by its Fn field when the test set one. Unset, Who answers a
// control principal, Accounts an empty listing, and every other call an
// error saying it is not set. Later tests configure it and never edit it.
type fakeBackend struct {
	mu  sync.Mutex
	log []shellCall

	WhoFn          func(ctx context.Context) (Principal, error)
	AccountsFn     func(ctx context.Context, how Refresh) (Listing, error)
	ChangeFn       func(ctx context.Context, id int, c Change) (wire.Account, error)
	RemoveFn       func(ctx context.Context, id int) error
	BeginLoginFn   func(ctx context.Context, b LoginBegin) (backendLogin, error)
	FinishLoginFn  func(ctx context.Context, id, code string) (LoginEnd, error)
	PasteKeyFn     func(ctx context.Context, key, label string) (LoginEnd, error)
	PasteTokenFn   func(ctx context.Context, account int, token string) (LoginEnd, error)
	AskFn          func(ctx context.Context, q Question, each func(message.Event)) (wire.End, error)
	SendRunFn      func(ctx context.Context, run, text string) error
	InterruptRunFn func(ctx context.Context, run string) error
	CloseRunFn     func(ctx context.Context, run string) error
}

// shellCall is one call a fakeBackend took: the method and its
// arguments after the context.
type shellCall struct {
	Method string
	Args   []any
}

// String is the call as Method(arg, …), a Refresh by its name.
func (c shellCall) String() string {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		switch a := a.(type) {
		case Refresh:
			args[i] = [...]string{"none", "stale", "force"}[a]
		default:
			args[i] = fmt.Sprint(a)
		}
	}
	return c.Method + "(" + strings.Join(args, ", ") + ")"
}

func (b *fakeBackend) took(method string, args ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.log = append(b.log, shellCall{Method: method, Args: args})
}

// calls is every call so far, as strings, in the order taken.
func (b *fakeBackend) calls() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.log))
	for i, c := range b.log {
		out[i] = c.String()
	}
	return out
}

// count is how many calls there have been.
func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.log)
}

// named is every call of one method, with its arguments.
func (b *fakeBackend) named(method string) []shellCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []shellCall
	for _, c := range b.log {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func shellUnset(method string) error {
	return errors.New("fakeBackend: " + method + " is not set")
}

func (b *fakeBackend) Who(ctx context.Context) (Principal, error) {
	b.took("Who")
	if b.WhoFn != nil {
		return b.WhoFn(ctx)
	}
	return Principal{Name: "tester", Role: "control", Via: "local"}, nil
}

func (b *fakeBackend) Accounts(ctx context.Context, how Refresh) (Listing, error) {
	b.took("Accounts", how)
	if b.AccountsFn != nil {
		return b.AccountsFn(ctx, how)
	}
	return Listing{}, nil
}

func (b *fakeBackend) Change(ctx context.Context, id int, c Change) (wire.Account, error) {
	b.took("Change", id, c)
	if b.ChangeFn != nil {
		return b.ChangeFn(ctx, id, c)
	}
	return wire.Account{}, shellUnset("Change")
}

func (b *fakeBackend) Remove(ctx context.Context, id int) error {
	b.took("Remove", id)
	if b.RemoveFn != nil {
		return b.RemoveFn(ctx, id)
	}
	return shellUnset("Remove")
}

func (b *fakeBackend) BeginLogin(ctx context.Context, lb LoginBegin) (backendLogin, error) {
	b.took("BeginLogin", lb)
	if b.BeginLoginFn != nil {
		return b.BeginLoginFn(ctx, lb)
	}
	return backendLogin{}, shellUnset("BeginLogin")
}

func (b *fakeBackend) FinishLogin(ctx context.Context, id, code string) (LoginEnd, error) {
	b.took("FinishLogin", id, code)
	if b.FinishLoginFn != nil {
		return b.FinishLoginFn(ctx, id, code)
	}
	return LoginEnd{}, shellUnset("FinishLogin")
}

func (b *fakeBackend) PasteKey(ctx context.Context, k, label string) (LoginEnd, error) {
	b.took("PasteKey", k, label)
	if b.PasteKeyFn != nil {
		return b.PasteKeyFn(ctx, k, label)
	}
	return LoginEnd{}, shellUnset("PasteKey")
}

func (b *fakeBackend) PasteToken(ctx context.Context, account int, token string) (LoginEnd, error) {
	b.took("PasteToken", account, token)
	if b.PasteTokenFn != nil {
		return b.PasteTokenFn(ctx, account, token)
	}
	return LoginEnd{}, shellUnset("PasteToken")
}

func (b *fakeBackend) Ask(ctx context.Context, q Question, each func(message.Event)) (wire.End, error) {
	b.took("Ask", q)
	if b.AskFn != nil {
		return b.AskFn(ctx, q, each)
	}
	return wire.End{}, shellUnset("Ask")
}

func (b *fakeBackend) SendRun(ctx context.Context, run, text string) error {
	b.took("SendRun", run, text)
	if b.SendRunFn != nil {
		return b.SendRunFn(ctx, run, text)
	}
	return shellUnset("SendRun")
}

func (b *fakeBackend) InterruptRun(ctx context.Context, run string) error {
	b.took("InterruptRun", run)
	if b.InterruptRunFn != nil {
		return b.InterruptRunFn(ctx, run)
	}
	return shellUnset("InterruptRun")
}

func (b *fakeBackend) CloseRun(ctx context.Context, run string) error {
	b.took("CloseRun", run)
	if b.CloseRunFn != nil {
		return b.CloseRunFn(ctx, run)
	}
	return shellUnset("CloseRun")
}

// assertNoIO fails the test if b takes any call while f runs: Update does
// no I/O, and only the commands it returns may call the Backend.
func assertNoIO(t *testing.T, b *fakeBackend, f func()) {
	t.Helper()
	before := b.count()
	f()
	if got := b.calls(); len(got) != before {
		t.Errorf("the Backend was called while Update ran: %q", got[before:])
	}
}

// shellRegistry gives the test a tab registry of its own, empty, and puts
// the package's back when the test ends.
func shellRegistry(t *testing.T) {
	t.Helper()
	saved := tabsRegistry
	tabsRegistry = nil
	t.Cleanup(func() { tabsRegistry = saved })
}

// shellModel is a model over b, on the fake clock, drawing without colour.
func shellModel(t *testing.T, b Backend, ticks *fakeTicks, with ...func(*Options)) *Model {
	t.Helper()
	p := colorprofile.Ascii
	o := Options{Backend: b, Tick: ticks.tick, Now: ticks.now, Loc: time.UTC, Profile: &p, Env: []string{}}
	for _, f := range with {
		f(&o)
	}
	return New(o)
}

// shellStart runs the model's Init and a first resize to 80×24.
func shellStart(t *testing.T, m *Model) []tea.Msg {
	t.Helper()
	_, seen := run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	return seen
}

// shellPing is a message no part of the shell knows.
type shellPing string

// shellTab is a tab that records what reaches it.
type shellTab struct {
	title  string
	keys   []string
	got    []string
	mice   []string // each mouse message, as "click 3,2"
	pastes []string
	typing bool
	busy   string
	// leaveFails makes Leave panic with it.
	leaveFails string
	e          *env
	sizes      chan<- [2]int        // each resize, when set
	held       chan<- struct{}      // told when b holds Update, when set
	hold       <-chan struct{}      // b holds Update until it closes, as tea.Exec holds the program
	left       chan<- time.Duration // the time Leave was given, when set
}

func (s *shellTab) Title() string { return s.title }

func (s *shellTab) Update(msg tea.Msg) (tab, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		s.keys = append(s.keys, msg.String())
		if s.hold != nil && msg.String() == "b" {
			s.held <- struct{}{}
			<-s.hold
		}
	case tea.WindowSizeMsg:
		if s.sizes != nil {
			select {
			case s.sizes <- [2]int{msg.Width, msg.Height}:
			default:
			}
		}
	case tea.MouseClickMsg:
		s.mice = append(s.mice, fmt.Sprintf("click %d,%d", msg.X, msg.Y))
	case tea.MouseWheelMsg:
		s.mice = append(s.mice, fmt.Sprintf("wheel %d,%d", msg.X, msg.Y))
	case tea.PasteMsg:
		s.pastes = append(s.pastes, msg.Content)
	case tea.PasteStartMsg:
		s.pastes = append(s.pastes, "<start>")
	case tea.PasteEndMsg:
		s.pastes = append(s.pastes, "<end>")
	case shellPing:
		s.got = append(s.got, string(msg))
	}
	return s, nil
}

func (s *shellTab) View(width, height int) string {
	return "tab " + s.title + " keys:" + strings.Join(s.keys, ",")
}

func (s *shellTab) Keys() []key.Binding {
	return []key.Binding{key.NewBinding(key.WithKeys("x"), key.WithHelp("x", "do"))}
}

func (s *shellTab) Typing() bool { return s.typing }
func (s *shellTab) Busy() string { return s.busy }

func (s *shellTab) Leave(ctx context.Context) {
	if s.leaveFails != "" {
		panic(s.leaveFails)
	}
	if s.left != nil {
		d := time.Duration(-1)
		if dl, ok := ctx.Deadline(); ok {
			d = time.Until(dl)
		}
		s.left <- d
	}
}

// shellTabs registers a tab at each place and returns them, by place.
func shellTabs(t *testing.T, places ...int) map[int]*shellTab {
	t.Helper()
	shellRegistry(t)
	tabs := map[int]*shellTab{}
	names := map[int]string{10: "Ten", 20: "Twenty", 30: "Thirty", 40: "Forty"}
	for _, p := range places {
		tb := &shellTab{title: names[p]}
		tabs[p] = tb
		register(p, func(e *env) tab { tb.e = e; return tb })
	}
	return tabs
}

// shellOverlay is an overlay that records what reaches it, and closes on
// esc or on shellPing(closeOn). As an asider it goes aside on esc instead,
// and back on Return.
type shellOverlay struct {
	keys, got []string
	mice      []string
	pastes    []string
	closeOn   string
	closed    bool
	asider    bool
	aside     bool
	pending   *backendLogin
}

func (o *shellOverlay) Update(msg tea.Msg) (overlay, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		o.keys = append(o.keys, msg.String())
		if msg.String() == "esc" {
			if o.asider {
				o.aside = true
			} else {
				o.closed = true
			}
		}
	case tea.MouseClickMsg:
		o.mice = append(o.mice, fmt.Sprintf("click %d,%d", msg.X, msg.Y))
	case tea.PasteMsg:
		o.pastes = append(o.pastes, msg.Content)
	case tea.PasteStartMsg:
		o.pastes = append(o.pastes, "<start>")
	case tea.PasteEndMsg:
		o.pastes = append(o.pastes, "<end>")
	case shellPing:
		o.got = append(o.got, string(msg))
		if o.closeOn != "" && string(msg) == o.closeOn {
			o.closed = true
		}
	}
	return o, nil
}

func (o *shellOverlay) View(width, height int) string {
	return "overlay keys:" + strings.Join(o.keys, ",")
}
func (o *shellOverlay) Keys() []key.Binding { return nil }
func (o *shellOverlay) Typing() bool        { return false }
func (o *shellOverlay) Closed() bool        { return o.closed }

// shellAsider is shellOverlay as the login flow is: it can be put aside.
type shellAsider struct{ *shellOverlay }

func (a shellAsider) Update(msg tea.Msg) (overlay, tea.Cmd) {
	a.shellOverlay.Update(msg)
	return a, nil
}
func (a shellAsider) Aside() bool                { return a.aside }
func (a shellAsider) Badge(now time.Time) string { return "login abc123 · aside" }
func (a shellAsider) Return()                    { a.aside = false }
func (a shellAsider) Pending() *backendLogin     { return a.pending }

// shellLeaving is an overlay that holds something open, as a page's
// listener: Run leaves it.
type shellLeaving struct {
	*shellOverlay
	left *bool
}

func (l shellLeaving) Update(msg tea.Msg) (overlay, tea.Cmd) {
	l.shellOverlay.Update(msg)
	return l, nil
}
func (l shellLeaving) Leave(context.Context) { *l.left = true }

func TestTabsRegisterInPlace(t *testing.T) {
	shellRegistry(t)
	register(30, func(e *env) tab { return &shellTab{title: "Thirty"} })
	register(10, func(e *env) tab { return &shellTab{title: "Ten"} })
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	header, _, _ := strings.Cut(plain(m), "\n")
	ten, thirty := strings.Index(header, "Ten"), strings.Index(header, "Thirty")
	if ten < 0 || thirty < 0 || ten > thirty {
		t.Errorf("the header is %q, want Ten before Thirty", header)
	}
	if !strings.Contains(plain(m), "tab Ten") {
		t.Errorf("the first place is not the tab shown:\n%s", plain(m))
	}
	defer func() {
		if recover() == nil {
			t.Error("a second tab at place 10 was taken")
		}
	}()
	register(10, func(e *env) tab { return &shellTab{title: "Again"} })
}

func TestKeysGoToTheFocusAndEverythingElseToAll(t *testing.T) {
	tabs := shellTabs(t, 10, 30)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	ten, thirty := tabs[10], tabs[30]

	run(t, m, press("x")[0], shellPing("one"))
	if !slices.Equal(ten.keys, []string{"x"}) || len(thirty.keys) != 0 {
		t.Errorf("the key reached %q and %q, want only the active tab", ten.keys, thirty.keys)
	}
	if !slices.Equal(ten.got, []string{"one"}) || !slices.Equal(thirty.got, []string{"one"}) {
		t.Errorf("the message reached %q and %q, want both", ten.got, thirty.got)
	}

	o := &shellOverlay{}
	run(t, m, openMsg{O: o}, press("y")[0], shellPing("two"))
	if !slices.Equal(o.keys, []string{"y"}) || !slices.Equal(ten.keys, []string{"x"}) {
		t.Errorf("with an overlay open the key reached %q and the tab %q", o.keys, ten.keys)
	}
	if !slices.Equal(o.got, []string{"two"}) || len(ten.got) != 2 || len(thirty.got) != 2 {
		t.Errorf("the message reached the overlay %q and the tabs %q %q", o.got, ten.got, thirty.got)
	}
	// An overlay over another: the top one has the keys, and both see the
	// rest. When the top one closes, the one under it has them again.
	top := &shellOverlay{}
	run(t, m, openMsg{O: top}, press("k")[0], shellPing("three"))
	if !slices.Equal(top.keys, []string{"k"}) || len(o.keys) != 1 || !slices.Equal(o.got, []string{"two", "three"}) {
		t.Errorf("stacked: top %q, under %q %q", top.keys, o.keys, o.got)
	}
	run(t, m, press("esc", "z")...)
	if !top.closed || !slices.Equal(o.keys, []string{"y", "z"}) {
		t.Errorf("after the top closed: under %q", o.keys)
	}
	run(t, m, press("esc", "w")...)
	if !o.closed || !slices.Equal(ten.keys, []string{"x", "w"}) {
		t.Errorf("after the overlay closed the key reached %q", ten.keys)
	}

	// A login flow put aside keeps hearing everything but keys.
	a := shellAsider{&shellOverlay{asider: true, pending: &backendLogin{ID: "abc123def"}}}
	run(t, m, openMsg{O: a}, press("1")[0])
	if !slices.Equal(a.keys, []string{"1"}) {
		t.Fatalf("the flow took %q", a.keys)
	}
	run(t, m, append(press("esc", "v"), shellPing("four"))...)
	if !slices.Equal(ten.keys, []string{"x", "w", "v"}) || !slices.Equal(a.got, []string{"four"}) {
		t.Errorf("aside: tab keys %q, the flow heard %q", ten.keys, a.got)
	}
	if !strings.Contains(plain(m), "login abc123") {
		t.Errorf("the badge is not shown:\n%s", plain(m))
	}
	if got := m.result().Pending; got == nil || got.ID != "abc123def" {
		t.Errorf("the result is %+v, want the parked login", got)
	}
	// Another flow while one is aside brings that one back (R17).
	other := shellAsider{&shellOverlay{asider: true}}
	run(t, m, openMsg{O: other}, press("u")[0])
	if a.aside || !slices.Equal(a.keys, []string{"1", "esc", "u"}) || len(other.keys) != 0 {
		t.Errorf("a second flow: the first took %q (aside %v), the second %q", a.keys, a.aside, other.keys)
	}
}

func TestTabsSwitchByNumberAndBracketUnlessTyping(t *testing.T) {
	tabs := shellTabs(t, 10, 30)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	shown := func() string {
		for _, name := range []string{"Ten", "Thirty"} {
			if strings.Contains(plain(m), "tab "+name) {
				return name
			}
		}
		return "none"
	}
	for _, step := range []struct{ key, want string }{
		{"3", "Thirty"}, {"1", "Ten"}, {"2", "Ten"}, {"4", "Ten"},
		{"]", "Thirty"}, {"]", "Ten"}, {"[", "Thirty"}, {"[", "Ten"},
	} {
		run(t, m, press(step.key)...)
		if got := shown(); got != step.want {
			t.Errorf("after %q the tab shown is %s, want %s", step.key, got, step.want)
		}
	}
	if len(tabs[10].keys)+len(tabs[30].keys) != 0 {
		t.Errorf("switching keys reached a tab: %q %q", tabs[10].keys, tabs[30].keys)
	}

	tabs[10].typing = true
	_, seen := run(t, m, press("3", "]", "[", "q", "M")...)
	if got := shown(); got != "Ten" || !slices.Equal(tabs[10].keys, []string{"3", "]", "[", "q", "M"}) {
		t.Errorf("typing: the tab shown is %s and took %q", got, tabs[10].keys)
	}
	if slices.Contains(seen, tea.Msg(tea.QuitMsg{})) || m.mouse {
		t.Error("typing q or M acted as a global key")
	}
	// ctrl+c is no text, so it quits even from a field: on a second press,
	// as q does (the chrome's two-step quit).
	if _, seen := run(t, m, press("ctrl+c", "ctrl+c")...); !slices.Contains(seen, tea.Msg(tea.QuitMsg{})) {
		t.Errorf("ctrl+c from a field: saw %v", seen)
	}

	tabs[10].typing = false
	if _, seen := run(t, m, press("q", "q")...); !slices.Contains(seen, tea.Msg(tea.QuitMsg{})) {
		t.Errorf("q saw %v, want a quit", seen)
	}
}

// A click or the wheel in the body reaches the view with focus, in the
// body's own coordinates; the rows of the chrome are the chrome's (F18).
func TestMouseReachesTheBodyInItsOwnCoordinates(t *testing.T) {
	tabs := shellTabs(t, 10, 30)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	top, height := m.body()
	if top != 1 || height != 24-2 {
		t.Fatalf("the body is %d rows from row %d", height, top)
	}
	run(t, m, tea.MouseClickMsg{X: 3, Y: top + 2}, tea.MouseWheelMsg{X: 4, Y: top, Button: tea.MouseWheelDown},
		tea.MouseClickMsg{X: 3, Y: 0}, tea.MouseClickMsg{X: 3, Y: 23})
	if want := []string{"click 3,2", "wheel 4,0"}; !slices.Equal(tabs[10].mice, want) || len(tabs[30].mice) != 0 {
		t.Errorf("the mouse reached %q and %q, want %q on the tab shown", tabs[10].mice, tabs[30].mice, want)
	}
	o := &shellOverlay{}
	run(t, m, openMsg{O: o}, tea.MouseClickMsg{X: 5, Y: top + 1})
	if !slices.Equal(o.mice, []string{"click 5,1"}) || len(tabs[10].mice) != 2 {
		t.Errorf("with an overlay open the mouse reached %q and the tab %q", o.mice, tabs[10].mice)
	}
}

func TestTooSmallSaysSoAndForgetsNothing(t *testing.T) {
	tabs := shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	run(t, m, press("a", "b")...)
	a := shellAsider{&shellOverlay{asider: true}}
	run(t, m, openMsg{O: a}, press("esc")[0])
	before := plain(m)
	for _, s := range [][2]int{{59, 14}, {60, 13}, {0, 0}, {1, 1}} {
		run(t, m, size(s[0], s[1]))
		want := fmt.Sprintf("rota needs a terminal of at least 60×14; this one is %d×%d", s[0], s[1])
		if got := plain(m); got != want {
			t.Errorf("at %d×%d the view is %q, want %q", s[0], s[1], got, want)
		}
		run(t, m, press("c")...)
		run(t, m, tea.MouseClickMsg{X: 0, Y: 2})
	}
	run(t, m, size(80, 24))
	if got := plain(m); got != before {
		t.Errorf("back at 80×24 the view is\n%s\nwant\n%s", got, before)
	}
	if !slices.Equal(tabs[10].keys, []string{"a", "b"}) || a.Closed() || !a.Aside() {
		t.Errorf("after being small the tab holds %q and the flow closed=%v aside=%v", tabs[10].keys, a.Closed(), a.Aside())
	}
}

func TestTheListingIsReadOnOpenAndKeptFresh(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	listing := Listing{Accounts: []wire.Account{{ID: 1, Provider: "claude"}}, Default: 1}
	b := &fakeBackend{AccountsFn: func(context.Context, Refresh) (Listing, error) { return listing, nil }}
	m := shellModel(t, b, ticks)
	seen := shellStart(t, m)
	got := b.calls()
	slices.Sort(got)
	if want := []string{"Accounts(stale)", "Who()"}; !slices.Equal(got, want) {
		t.Fatalf("on open the Backend took %q, want %q", got, want)
	}
	if !slices.ContainsFunc(seen, func(msg tea.Msg) bool { l, ok := msg.(listingMsg); return ok && l.How == RefreshStale }) {
		t.Errorf("no listingMsg among %v", seen)
	}
	if m.e.list.Default != 1 || len(m.e.list.Accounts) != 1 || m.e.who.Name != "tester" {
		t.Errorf("the env holds %+v and %+v", m.e.list, m.e.who)
	}

	n := b.count()
	run(t, m, ticks.advance(15*time.Second)...)
	if got := b.calls()[n:]; !slices.Equal(got, []string{"Accounts(none)"}) {
		t.Errorf("after 15 s the Backend took %q, want the cached listing", got)
	}
	n = b.count()
	run(t, m, ticks.advance(2*time.Minute-15*time.Second)...)
	if got := b.calls()[n:]; !slices.Contains(got, "Accounts(stale)") {
		t.Errorf("after 2 min the Backend took %q, want a stale read", got)
	}
	// Each timer is set again when it fires.
	n = b.count()
	run(t, m, ticks.advance(15*time.Second)...)
	if got := b.calls()[n:]; !slices.Equal(got, []string{"Accounts(none)"}) {
		t.Errorf("15 s later the Backend took %q, want the cached listing again", got)
	}
	n = b.count()
	run(t, m, ticks.advance(2*time.Minute)...)
	if got := b.calls()[n:]; !slices.Contains(got, "Accounts(stale)") {
		t.Errorf("2 min later the Backend took %q, want a stale read again", got)
	}
	n = b.count()
	run(t, m, refreshMsg{How: RefreshForce})
	if got := b.calls()[n:]; !slices.Equal(got, []string{"Accounts(force)"}) {
		t.Errorf("a forced refresh took %q", got)
	}
}

func TestABackendFailureIsAToastNotACrash(t *testing.T) {
	tabs := shellTabs(t, 10)
	ticks := &fakeTicks{}
	b := &fakeBackend{
		AccountsFn: func(context.Context, Refresh) (Listing, error) { return Listing{}, errors.New("the daemon went away") },
		WhoFn:      func(context.Context) (Principal, error) { return Principal{}, errors.New("no answer") },
	}
	m := shellModel(t, b, ticks)
	seen := shellStart(t, m)
	var toasts []string
	for _, msg := range seen {
		if tm, ok := msg.(toastMsg); ok && tm.Level == levelErr {
			toasts = append(toasts, tm.Text)
		}
	}
	if !slices.ContainsFunc(toasts, func(s string) bool { return strings.Contains(s, "the daemon went away") }) ||
		!slices.ContainsFunc(toasts, func(s string) bool { return strings.Contains(s, "no answer") }) {
		t.Errorf("the errors were not toasted: %q", toasts)
	}
	if !strings.Contains(plain(m), "the daemon went away") && !strings.Contains(plain(m), "no answer") {
		t.Errorf("no toast is drawn:\n%s", plain(m))
	}
	n := len(b.named("Accounts"))
	run(t, m, ticks.advance(15*time.Second)...)
	if len(b.named("Accounts")) != n+1 || len(b.named("Who")) != 2 {
		t.Errorf("the next tick did not try again: %q", b.calls())
	}
	run(t, m, press("x")...)
	if !slices.Equal(tabs[10].keys, []string{"x"}) {
		t.Errorf("keys stopped working: %q", tabs[10].keys)
	}
}

func TestTheDaemonIsPolled(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	var mu sync.Mutex
	asked := 0
	daemon := func(ctx context.Context) Daemon {
		mu.Lock()
		defer mu.Unlock()
		asked++
		if _, ok := ctx.Deadline(); !ok {
			return Daemon{Err: "no deadline"}
		}
		return Daemon{Running: asked == 1, PID: 42}
	}
	m := shellModel(t, &fakeBackend{}, ticks, func(o *Options) { o.Daemon = daemon })
	seen := shellStart(t, m)
	_, more := run(t, m, ticks.advance(5*time.Second)...)
	var got []Daemon
	for _, msg := range append(seen, more...) {
		if d, ok := msg.(daemonMsg); ok {
			got = append(got, d.Daemon)
		}
	}
	if len(got) != 2 || !got[0].Running || got[1].Running || got[1].Err != "" {
		t.Errorf("the daemon messages are %+v, want running and then not", got)
	}
	if m.e.daemon.Running {
		t.Errorf("the env still says the daemon runs")
	}
}

func TestEffectsBecomeTerminalCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	before := plain(m)
	c := execTail(exec.Command("nothing"))
	done := func(error) tea.Msg { return nil }
	if got, ok := handOff(c, done)().(handOffMsg); !ok || got.Cmd != c || got.Done == nil {
		t.Errorf("handOff gave %+v", got)
	}
	if got := copyText("abc")(); got != (clipboardMsg{Text: "abc"}) {
		t.Errorf("copyText gave %+v", got)
	}
	for _, e := range []struct {
		msg  tea.Msg
		want tea.Msg // what Bubble Tea's own command gives, or nil for any of its own
	}{
		{handOffMsg{Cmd: c, Done: done}, nil},
		{clipboardMsg{Text: "abc"}, tea.SetClipboard("abc")()},
		{bellMsg{}, tea.Raw("\a")()},
	} {
		_, cmd := m.Update(e.msg)
		if cmd == nil {
			t.Errorf("%T gave no command", e.msg)
			continue
		}
		got := cmd()
		if s := fmt.Sprintf("%T", got); !strings.HasPrefix(s, "tea.") {
			t.Errorf("%T became %s, not one of Bubble Tea's own", e.msg, s)
		}
		if e.want != nil && got != e.want {
			t.Errorf("%T became %#v, want %#v", e.msg, got, e.want)
		}
		if plain(m) != before {
			t.Errorf("%T changed the view", e.msg)
		}
	}
	// Nothing to hand the terminal to never reaches Bubble Tea, which would
	// panic on it and end the program, and Done still hears of it.
	var heard error
	_, cmd := m.Update(handOffMsg{Done: func(err error) tea.Msg { heard = err; return shellPing("ended") }})
	if cmd == nil {
		t.Fatal("a hand-off of nothing never told its Done")
	}
	if got := cmd(); got != shellPing("ended") || heard == nil {
		t.Errorf("a hand-off of nothing gave %v and Done heard %v, want Done's message after an error", got, heard)
	}
	if _, cmd := m.Update(handOffMsg{}); cmd != nil {
		t.Errorf("a hand-off of nothing, with no Done, gave %v", cmd())
	}
}

func TestOpenURLUsesTheBrowserOrSaysThereIsNone(t *testing.T) {
	shellTabs(t, 10)
	var opened []string
	m := shellModel(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) {
		o.OpenURL = func(u string) error {
			opened = append(opened, u)
			if u == "bad" {
				return errors.New("no display")
			}
			return nil
		}
	})
	if got := openURL(m.e, "https://x")(); got != nil || !slices.Equal(opened, []string{"https://x"}) {
		t.Errorf("open gave %v, opened %q", got, opened)
	}
	if got, ok := openURL(m.e, "bad")().(toastMsg); !ok || !strings.Contains(got.Text, "no display") {
		t.Errorf("a failed open gave %v", got)
	}
	m = shellModel(t, &fakeBackend{}, &fakeTicks{})
	if got, ok := openURL(m.e, "https://x")().(toastMsg); !ok || got.Level != levelWarn {
		t.Errorf("with no browser: %v", got)
	}
}

func TestExecTailKeepsTheLastWords(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	path := fakecli.Install(t, bin, "tailer", fakecli.Spec{
		Stderr: []string{strings.Repeat("x", 6<<10), "boom"},
		Exit:   3,
	})
	tl := execTail(exec.Command(path))
	var errOut bytes.Buffer
	tl.SetStdin(strings.NewReader(""))
	tl.SetStdout(io.Discard)
	tl.SetStderr(&errOut)
	var exit *exec.ExitError
	if err := tl.Run(); !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("the run ended %v, want exit 3", err)
	}
	if got := tl.Last(); got != "boom" {
		t.Errorf("Last is %q", got)
	}
	if want := strings.Repeat("x", 6<<10) + "\nboom\n"; strings.ReplaceAll(errOut.String(), "\r\n", "\n") != want {
		t.Errorf("the program's writer got %d bytes, want %d", errOut.Len(), len(want))
	}
	if n := tl.tail.len(); n > 4<<10 {
		t.Errorf("the tail holds %d bytes", n)
	}
	// With no writer set, the tail still keeps what was said.
	tl = execTail(exec.Command(path))
	tl.Run()
	if got := tl.Last(); got != "boom" {
		t.Errorf("unset stderr: Last is %q", got)
	}
}

func TestWatchGatesControlKeys(t *testing.T) {
	for _, role := range []string{"watch", "control"} {
		t.Run(role, func(t *testing.T) {
			shellTabs(t, 10)
			b := &fakeBackend{WhoFn: func(context.Context) (Principal, error) {
				return Principal{Name: "p", Role: role, Via: "ssh"}, nil
			}}
			m := shellModel(t, b, &fakeTicks{})
			gated := func() bool {
				k := key.NewBinding(key.WithKeys("n"))
				m.e.gate(&k)
				return !k.Enabled()
			}
			if !gated() {
				t.Error("before Who answers a control key is enabled")
			}
			shellStart(t, m)
			if got := gated(); got != (role == "watch") {
				t.Errorf("gated is %v", got)
			}
		})
	}
}

func TestUpdateDoesNoIO(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	shellTabs(t, 10, 30)
	ticks := &fakeTicks{}
	var mu sync.Mutex
	daemonAsked := 0
	b := &fakeBackend{}
	m := shellModel(t, b, ticks, func(o *Options) {
		o.Daemon = func(context.Context) Daemon { mu.Lock(); daemonAsked++; mu.Unlock(); return Daemon{} }
		o.OpenURL = func(string) error { t.Error("a browser was opened by Update"); return nil }
	})
	c := execTail(exec.Command("nothing"))
	msgs := []tea.Msg{
		size(80, 24), press("x")[0], press("3")[0], press("]")[0], press("M")[0],
		refreshMsg{How: RefreshForce}, listingMsg{How: RefreshNone}, listingMsg{Err: errors.New("x")},
		daemonMsg{Daemon: Daemon{Running: true}}, selectMsg{Account: 1}, toastMsg{Text: "t"},
		openMsg{O: &shellOverlay{}}, press("y")[0], press("esc")[0],
		handOffMsg{Cmd: c, Done: func(error) tea.Msg { return nil }}, clipboardMsg{Text: "c"}, bellMsg{},
		modelPollMsg{Poll: modelPollList}, modelPollMsg{Poll: modelPollStale}, modelPollMsg{Poll: modelPollDaemon},
		modelWhoMsg{Who: Principal{Role: "control"}}, tea.BackgroundColorMsg{}, tea.MouseClickMsg{X: 1, Y: 3},
		tea.MouseWheelMsg{X: 1, Y: 3, Button: tea.MouseWheelDown}, tea.PasteMsg{Content: "p"},
		handOffMsg{}, size(30, 10), press("q")[0],
	}
	assertNoIO(t, b, func() {
		m.Init()
		for _, msg := range msgs {
			m.Update(msg)
			m.View()
		}
	})
	if daemonAsked != 0 {
		t.Errorf("the daemon was asked %d times by Update", daemonAsked)
	}
	run(t, m, size(80, 24), refreshMsg{How: RefreshForce})
	if b.count() == 0 {
		t.Error("the commands Update returned called nothing")
	}
}

// What a stub returns until its task fills it in is safe to hand the
// shell: a closed overlay is never opened, and a placeholder step draws
// and takes messages (F14).
func TestStubsAreSafeToOpen(t *testing.T) {
	tabs := shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	run(t, m, openMsg{O: overlayClosed()}, openMsg{O: nil}, press("x")[0])
	if len(m.stack) != 0 || !slices.Equal(tabs[10].keys, []string{"x"}) {
		t.Errorf("a stub's overlay was opened: stack %d, tab keys %q", len(m.stack), tabs[10].keys)
	}
	s := overlayNotYet()
	s, cmd := s.Update(press("enter")[0])
	if s == nil || cmd != nil || s.View(80) != "not yet" || s.Typing() || len(s.Keys()) != 0 {
		t.Errorf("the placeholder step is %#v", s)
	}
}

// A call makes one context, which ends with it, so the program's context
// holds none for long; and it answers for a callee that ignores its
// deadline or panics, so no kind of call stays in flight for good.
func TestCallMakesOneContextAndAnswersForItsCallee(t *testing.T) {
	parent := &shellParent{done: make(chan struct{})}
	// The canary: a child left open is counted and cancelling it uncounts
	// it, so the count below means something on this Go.
	_, leak := context.WithCancel(parent)
	if n := parent.registered(); n != 1 {
		t.Fatalf("a child left open registered %d times on its parent; this Go no longer uses AfterFunc, and the test needs another way in", n)
	}
	leak()
	if n := parent.registered(); n != 0 {
		t.Fatalf("a cancelled child is still registered: %d", n)
	}
	e := &env{ctx: parent}
	for range 1000 {
		e.call(envCallTimeout, func(context.Context) tea.Msg { return nil })()
		e.call(0, func(context.Context) tea.Msg { return nil })()
	}
	if n := parent.registered(); n != 0 {
		t.Errorf("after 2000 finished calls %d children are still registered on the program's context", n)
	}

	failed := func(err error) tea.Msg { return shellPing("failed: " + err.Error()) }
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	got := e.callOr(20*time.Millisecond, func(context.Context) tea.Msg { <-block; return shellPing("late") }, failed, nil)()
	if took := time.Since(start); got != shellPing("failed: no answer: context deadline exceeded") || took > envCallGrace+2*time.Second {
		t.Errorf("a callee that ignored its deadline gave %v after %v", got, took)
	}
	got = e.callOr(20*time.Millisecond, func(ctx context.Context) tea.Msg { <-ctx.Done(); return shellPing("its own") }, failed, nil)()
	if got != shellPing("its own") {
		t.Errorf("a callee that saw its deadline gave %v, want its own answer", got)
	}
	got = e.callOr(time.Second, func(context.Context) tea.Msg { panic("boom") }, failed, nil)()
	if got != shellPing("failed: the call failed: boom") {
		t.Errorf("a callee that panicked gave %v", got)
	}
	if got, ok := e.call(time.Second, func(context.Context) tea.Msg { panic("boom") })().(toastMsg); !ok || got.Level != levelErr || !strings.Contains(got.Text, "boom") {
		t.Errorf("call's own answer for a panic is %v", got)
	}
}

// shellParent is a context of the test's own that counts the children
// registered on it, through the public AfterFunc convention: a parent that
// is no *cancelCtx (Value answers nil) but has AfterFunc is asked to
// schedule each child's cancel, and a child that ends calls the stop it was
// given.
type shellParent struct {
	done chan struct{}
	mu   sync.Mutex
	live int
}

func (p *shellParent) Deadline() (time.Time, bool) { return time.Time{}, false }
func (p *shellParent) Done() <-chan struct{}       { return p.done }
func (p *shellParent) Value(any) any               { return nil }
func (p *shellParent) Err() error {
	select {
	case <-p.done:
		return context.Canceled
	default:
		return nil
	}
}

func (p *shellParent) AfterFunc(func()) func() bool {
	p.mu.Lock()
	p.live++
	p.mu.Unlock()
	var once sync.Once
	return func() bool {
		stopped := false
		once.Do(func() { p.mu.Lock(); p.live--; p.mu.Unlock(); stopped = true })
		return stopped
	}
}

func (p *shellParent) registered() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.live
}

// A callee that ignores its context is answered for at once and counted
// as parked until it really returns; a timer sends no more of its kind
// meanwhile, so at most one such goroutine is ever left (M2).
func TestACalleeThatIgnoresItsContextIsSentOnceAtATime(t *testing.T) {
	e := &env{ctx: context.Background()}
	var parked atomic.Int32
	release := make(chan struct{})
	returned := make(chan struct{})
	got := e.callOr(20*time.Millisecond, func(context.Context) tea.Msg {
		<-release
		defer close(returned)
		return shellPing("late")
	}, func(err error) tea.Msg { return shellPing("failed") }, &parked)()
	if got != shellPing("failed") || parked.Load() != 1 {
		t.Fatalf("answered %v with %d parked, want the failure and one parked", got, parked.Load())
	}
	close(release)
	<-returned
	for deadline := time.Now().Add(5 * time.Second); parked.Load() != 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if n := parked.Load(); n != 0 {
		t.Errorf("once the callee returned %d are still parked", n)
	}
	// One that answers in time is never counted.
	e.callOr(time.Second, func(context.Context) tea.Msg { return nil }, func(error) tea.Msg { return nil }, &parked)()
	if n := parked.Load(); n != 0 {
		t.Errorf("a callee that answered left %d parked", n)
	}

	// The timers skip a kind that has one parked, and send it again at zero.
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	b := &fakeBackend{WhoFn: func(context.Context) (Principal, error) { return Principal{}, errors.New("no answer") }}
	var mu sync.Mutex
	asked := 0
	m := shellModel(t, b, ticks, func(o *Options) {
		o.Daemon = func(context.Context) Daemon { mu.Lock(); asked++; mu.Unlock(); return Daemon{} }
	})
	shellStart(t, m)
	m.parkedWho.Store(1)
	m.parkedList[RefreshNone].Store(1)
	m.parkedList[RefreshStale].Store(1)
	m.parkedDaemon.Store(1)
	n := b.count()
	run(t, m, ticks.advance(2*time.Minute)...)
	mu.Lock()
	daemonAsked := asked
	mu.Unlock()
	if got := b.calls()[n:]; len(got) != 0 || daemonAsked != 1 {
		t.Errorf("with each kind parked the timers sent %q and asked the daemon %d times", got, daemonAsked)
	}
	m.parkedWho.Store(0)
	m.parkedList[RefreshNone].Store(0)
	m.parkedList[RefreshStale].Store(0)
	m.parkedDaemon.Store(0)
	run(t, m, ticks.advance(2*time.Minute)...)
	sent := b.calls()[n:]
	for _, want := range []string{"Who()", "Accounts(none)", "Accounts(stale)"} {
		if !slices.Contains(sent, want) {
			t.Errorf("at zero the timers sent %q, want %s among them", sent, want)
		}
	}
	mu.Lock()
	daemonAsked = asked
	mu.Unlock()
	if daemonAsked < 2 {
		t.Error("at zero the daemon was not asked again")
	}
}

// A Who or a stale read that fails without answering still says so, keeps
// its own kind, and is sent again by the next timer.
func TestAWhoOrAListingThatPanicsIsAskedAgain(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	var mu sync.Mutex
	whoAsked, staleAsked := 0, 0
	b := &fakeBackend{
		WhoFn: func(context.Context) (Principal, error) {
			mu.Lock()
			whoAsked++
			n := whoAsked
			mu.Unlock()
			if n == 1 {
				panic("who broke")
			}
			return Principal{Name: "p", Role: "control"}, nil
		},
		AccountsFn: func(_ context.Context, how Refresh) (Listing, error) {
			if how != RefreshStale {
				return Listing{}, nil
			}
			mu.Lock()
			staleAsked++
			n := staleAsked
			mu.Unlock()
			if n == 1 {
				panic("the read broke")
			}
			return Listing{Default: 7}, nil
		},
	}
	m := shellModel(t, b, ticks)
	seen := shellStart(t, m)
	var toasts []string
	for _, msg := range seen {
		if tm, ok := msg.(toastMsg); ok {
			toasts = append(toasts, tm.Text)
		}
	}
	if !slices.ContainsFunc(toasts, func(s string) bool { return strings.Contains(s, "who broke") }) ||
		!slices.ContainsFunc(toasts, func(s string) bool { return strings.Contains(s, "the read broke") }) {
		t.Errorf("the failures were said as %q", toasts)
	}
	if m.whoKnown || m.e.control() || m.listing[RefreshStale] != 0 {
		t.Errorf("after the panics: who known %v, control %v, stale reads out %d", m.whoKnown, m.e.control(), m.listing[RefreshStale])
	}
	run(t, m, ticks.advance(2*time.Minute)...)
	if !m.whoKnown || !m.e.control() || m.e.list.Default != 7 {
		t.Errorf("the next timers did not ask again: who known %v, control %v, listing %+v", m.whoKnown, m.e.control(), m.e.list)
	}
}

// The header never stays at a daemon that is gone: a Daemon func that
// panics is a daemon that does not run, and the next poll asks again.
func TestADaemonThatFailsIsNotRunning(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	var mu sync.Mutex
	asked := 0
	daemon := func(context.Context) Daemon {
		mu.Lock()
		asked++
		n := asked
		mu.Unlock()
		if n == 2 {
			panic("ctl.sock went away")
		}
		return Daemon{Running: true}
	}
	m := shellModel(t, &fakeBackend{}, ticks, func(o *Options) { o.Daemon = daemon })
	shellStart(t, m)
	if !m.e.daemon.Running {
		t.Fatal("the first answer was not taken")
	}
	run(t, m, ticks.advance(5*time.Second)...)
	if m.e.daemon.Running || !strings.Contains(m.e.daemon.Err, "ctl.sock went away") || !strings.Contains(plain(m), "no daemon") {
		t.Errorf("after a panic the daemon is %+v and the header\n%s", m.e.daemon, plain(m))
	}
	run(t, m, ticks.advance(5*time.Second)...)
	if !m.e.daemon.Running || asked != 3 {
		t.Errorf("the next poll did not ask again: asked %d, %+v", asked, m.e.daemon)
	}
}

// R29, with the listing's own deadline for a read that refreshes.
func TestEveryCallHasItsDeadline(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	var mu sync.Mutex
	got := map[string]time.Duration{}
	took := func(name string, ctx context.Context) {
		d := time.Duration(-1)
		if dl, ok := ctx.Deadline(); ok {
			d = time.Until(dl)
		}
		mu.Lock()
		got[name] = d
		mu.Unlock()
	}
	b := &fakeBackend{
		WhoFn: func(ctx context.Context) (Principal, error) { took("Who", ctx); return Principal{Role: "control"}, nil },
		AccountsFn: func(ctx context.Context, how Refresh) (Listing, error) {
			took(shellCall{Method: "Accounts", Args: []any{how}}.String(), ctx)
			return Listing{}, nil
		},
	}
	m := shellModel(t, b, ticks, func(o *Options) {
		o.Daemon = func(ctx context.Context) Daemon { took("Daemon", ctx); return Daemon{} }
	})
	shellStart(t, m)
	run(t, m, refreshMsg{How: RefreshForce})
	run(t, m, ticks.advance(15*time.Second)...)
	want := map[string]time.Duration{
		"Who": 30 * time.Second, "Accounts(none)": 30 * time.Second, "Daemon": 30 * time.Second,
		"Accounts(stale)": 60 * time.Second, "Accounts(force)": 60 * time.Second,
	}
	for name, w := range want {
		if d, ok := got[name]; !ok || d > w || d < w-5*time.Second {
			t.Errorf("%s had %v to answer, want %v", name, d, w)
		}
	}
}

// A timer that fires while its call is still out sends no second one, and
// sends again once the answer has come.
func TestATimerSkipsACallStillOut(t *testing.T) {
	shellTabs(t, 10)
	ticks := &fakeTicks{}
	release := make(chan struct{})
	var mu sync.Mutex
	daemonAsked := 0
	b := &fakeBackend{AccountsFn: func(ctx context.Context, how Refresh) (Listing, error) {
		<-release
		return Listing{}, nil
	}}
	m := shellModel(t, b, ticks, func(o *Options) {
		o.Daemon = func(context.Context) Daemon {
			mu.Lock()
			daemonAsked++
			mu.Unlock()
			<-release
			return Daemon{}
		}
	})
	run(t, m, tea.BatchMsg{m.Init()}, size(80, 24))
	for range 3 {
		run(t, m, ticks.advance(15*time.Second)...)
	}
	run(t, m, ticks.advance(2*time.Minute)...)
	mu.Lock()
	asked := daemonAsked
	mu.Unlock()
	if none, stale := len(slices.DeleteFunc(b.calls(), func(s string) bool { return s != "Accounts(none)" })),
		len(slices.DeleteFunc(b.calls(), func(s string) bool { return s != "Accounts(stale)" })); none != 1 || stale != 1 || asked != 1 {
		t.Errorf("with each call still out: %d cached reads, %d stale reads, %d daemon asks; want one each", none, stale, asked)
	}
	close(release)
	for i := 0; i < 5 && len(harnessDriverOf(t).leftover()) > 0; i++ {
		await(t, m)
	}
	n := b.count()
	run(t, m, ticks.advance(15*time.Second)...)
	if got := b.calls()[n:]; !slices.Contains(got, "Accounts(none)") {
		t.Errorf("once answered, the next tick sent %q", got)
	}
}

// Locally the model keeps the attach's window. Over SSH Run owns it, and a
// resize that differs from it is stale: the model answers it with the
// latest, and leaves the window alone.
func TestTheWindowIsTheModelsLocallyAndRunsOverSSH(t *testing.T) {
	shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	run(t, m, size(100, 30))
	if c, r := m.e.win.size(); c != 100 || r != 30 {
		t.Errorf("locally the window is %d×%d after a resize to 100×30", c, r)
	}
	m = shellModel(t, &fakeBackend{}, &fakeTicks{}, func(o *Options) {
		o.Resize, o.Remote, o.Cols, o.Rows = make(chan [2]int), true, 80, 24
	})
	m.e.win.set(120, 40) // as Run's reader does when a resize arrives
	_, seen := run(t, m, size(80, 24))
	if !slices.Contains(seen, tea.Msg(tea.WindowSizeMsg{Width: 120, Height: 40})) {
		t.Errorf("a stale size was not answered with the latest: saw %v", seen)
	}
	if c, r := m.e.win.size(); c != 120 || r != 40 || m.e.width != 120 || m.e.height != 40 {
		t.Errorf("the window is %d×%d and the model %d×%d, want 120×40", c, r, m.e.width, m.e.height)
	}
}

// A paste is typed input: it reaches only what has focus, never a hidden
// tab or a login flow put aside (I2).
func TestAPasteGoesOnlyToTheFocus(t *testing.T) {
	tabs := shellTabs(t, 10, 30)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	flow := shellAsider{&shellOverlay{asider: true}}
	run(t, m, openMsg{O: flow}, press("esc")[0], press("3")[0],
		tea.PasteStartMsg{}, tea.PasteMsg{Content: "question for Ask"}, tea.PasteEndMsg{})
	want := []string{"<start>", "question for Ask", "<end>"}
	if !slices.Equal(tabs[30].pastes, want) || len(tabs[10].pastes) != 0 || len(flow.pastes) != 0 {
		t.Errorf("the paste reached the shown tab %q, the hidden one %q and the flow aside %q", tabs[30].pastes, tabs[10].pastes, flow.pastes)
	}
	o := &shellOverlay{}
	run(t, m, openMsg{O: o}, tea.PasteStartMsg{}, tea.PasteMsg{Content: "code"}, tea.PasteEndMsg{})
	if !slices.Equal(o.pastes, []string{"<start>", "code", "<end>"}) || len(tabs[30].pastes) != 3 {
		t.Errorf("with an overlay open the paste reached it %q and the tab %q", o.pastes, tabs[30].pastes)
	}
	run(t, m, size(40, 10), tea.PasteStartMsg{}, tea.PasteMsg{Content: "unseen"}, tea.PasteEndMsg{}, size(80, 24))
	if len(o.pastes) != 3 {
		t.Errorf("a paste while too small reached %q", o.pastes)
	}
}

// One flow at a time (R17) holds with the flow open as well as aside, and
// the result is its parked login either way (R18).
func TestOneLoginFlowAtATime(t *testing.T) {
	tabs := shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	shellStart(t, m)
	first := shellAsider{&shellOverlay{asider: true, pending: &backendLogin{ID: "first"}}}
	run(t, m, openMsg{O: first})
	if got := m.result().Pending; got == nil || got.ID != "first" {
		t.Errorf("with the flow open the result is %+v", got)
	}
	second := shellAsider{&shellOverlay{asider: true, pending: &backendLogin{ID: "second"}}}
	run(t, m, openMsg{O: second}, press("k")[0])
	if !slices.Equal(first.keys, []string{"k"}) || len(second.keys) != 0 || len(m.stack) != 1 {
		t.Errorf("a second flow over an open one: first %q, second %q, %d open", first.keys, second.keys, len(m.stack))
	}
	// An overlay that a message closes gives the keys back at once.
	o := &shellOverlay{closeOn: "done"}
	run(t, m, openMsg{O: o}, shellPing("done"), press("j")[0])
	if !o.closed || !slices.Equal(first.keys, []string{"k", "j"}) || len(tabs[10].keys) != 0 {
		t.Errorf("after a message closed the top: under it %q, tab %q", first.keys, tabs[10].keys)
	}
}

// Every goroutine the shell starts and every Leave recovers (F31): a panic
// there is said, and the rest still runs.
func TestPanicsInTheShellsOwnGoroutinesAreSaid(t *testing.T) {
	var mu sync.Mutex
	var sent []tea.Msg
	send := func(msg tea.Msg) { mu.Lock(); sent = append(sent, msg); mu.Unlock() }
	var wg sync.WaitGroup
	programGo(&wg, send, "testing", func() { panic("boom") })
	wg.Wait()
	if len(sent) != 1 || sent[0] != (toastMsg{Text: "testing failed: boom", Level: levelErr}) {
		t.Errorf("the panic was said as %v", sent)
	}

	tabs := shellTabs(t, 10, 30)
	tabs[10].leaveFails = "no way out"
	left := make(chan time.Duration, 1)
	tabs[30].left = left
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	overlayLeft, asideLeft := false, false
	m.stack = []overlay{shellLeaving{&shellOverlay{}, &overlayLeft}, &shellOverlay{}}
	// The flow put aside holds what Task 17's page holds open.
	m.aside = shellLeavingAsider{shellAsider{&shellOverlay{asider: true, aside: true}}, &asideLeft}
	err := programLeave(m.leavers())
	if err == nil || err.Error() != "tui: leaving Ten: no way out" {
		t.Errorf("Leave's panic is %q, want one line naming the tab", err)
	}
	if len(left) != 1 || !overlayLeft || !asideLeft {
		t.Errorf("after one panicked the rest were not left: tab %d, overlay %v, flow aside %v", len(left), overlayLeft, asideLeft)
	}
}

// shellLeavingAsider is a login flow that holds something open.
type shellLeavingAsider struct {
	shellAsider
	left *bool
}

func (l shellLeavingAsider) Update(msg tea.Msg) (overlay, tea.Cmd) {
	l.shellAsider.Update(msg)
	return l, nil
}
func (l shellLeavingAsider) Leave(context.Context) { *l.left = true }

func TestTheBackgroundPicksTheTheme(t *testing.T) {
	shellTabs(t, 10)
	m := shellModel(t, &fakeBackend{}, &fakeTicks{})
	run(t, m, tea.BackgroundColorMsg{Color: color.White})
	if m.e.th.Dark {
		t.Error("a white background gave the dark theme")
	}
	run(t, m, tea.BackgroundColorMsg{}, tea.BackgroundColorMsg{Color: color.Black})
	if !m.e.th.Dark {
		t.Error("a black background gave the light theme")
	}
}

// A process the command leaves behind holding its stderr does not keep
// the TUI from coming back (I4).
func TestExecTailDoesNotWaitForWhatTheCommandLeftBehind(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	ready := filepath.Join(t.TempDir(), "helper.pid")
	helper := fakecli.Install(t, bin, "helper", fakecli.Spec{Ready: ready, Sleep: "30s"})
	cli := fakecli.Install(t, bin, "cli", fakecli.Spec{Detach: helper, Stderr: []string{"bye"}})
	tl := execTail(exec.Command(cli))
	tl.SetStdin(strings.NewReader(""))
	tl.SetStdout(io.Discard)
	tl.SetStderr(io.Discard)
	start := time.Now()
	err := tl.Run()
	took := time.Since(start)
	pid := fakecli.WaitReady(t, ready)
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			p.Kill()
		}
	})
	if err != nil || took > effectsWaitDelay+5*time.Second {
		t.Errorf("Run returned %v after %v, with the helper still holding stderr", err, took)
	}
	if got := tl.Last(); got != "bye" {
		t.Errorf("Last is %q", got)
	}
}
