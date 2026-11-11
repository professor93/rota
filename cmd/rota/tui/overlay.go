package tui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// overlay is a view over the tabs: a form, a confirm, the login flow. A
// view opens one with env.open, as its Update returns, or a command with an
// openMsg. Overlays stack: the top one has the keys, pastes and the mouse,
// every open one hears every other message, and one that says it is Closed
// is dropped, giving the keys back to the one under it, or to the tab.
// What the shell may also ask of one, each optional: Init() tea.Cmd, run
// once as it is opened (modelStarter); Busy() string, what quitting ends,
// named in the quit ask; and, for one that holds something open (a page's
// listener), Leave(ctx context.Context), which Run calls after the program
// ends, as it does a tab's.
type overlay interface {
	Update(msg tea.Msg) (overlay, tea.Cmd)
	// View draws the overlay in the space the chrome leaves, as a tab's.
	View(width, height int) string
	Keys() []key.Binding
	// Typing says a field has focus, as a tab's Typing does.
	Typing() bool
	Closed() bool
}

// asider is an overlay the shell can keep while it is put aside: the login
// flow. Put aside, it gets no keys or pastes and still hears every other
// message, and its Badge is shown in the footer. An asider is a pointer:
// Return changes it in place, and a copy would stay aside.
type asider interface {
	overlay
	// Aside says the flow is put aside. The shell takes it off the stack
	// when it says so after a message.
	Aside() bool
	Badge(now time.Time) string
	// Return brings it back. The shell calls it when another flow is
	// opened while this one is aside (R17): one flow at a time.
	Return()
	// Pending is the login it holds on the server, or nil.
	Pending() *backendLogin
}

// step is one stage of the login flow: the code, the device, the page.
type step interface {
	// Init starts what the step runs by itself: a device login's first
	// question, the page's look at this machine's addresses. The flow
	// calls it once, when it shows the step; nil where nothing starts.
	Init() tea.Cmd
	Update(msg tea.Msg) (step, tea.Cmd)
	View(width int) string
	Keys() []key.Binding
	Typing() bool
}

// stepDone says a step is finished: with End, or with an Err nothing will
// put right.
type stepDone struct {
	End LoginEnd
	Err error
}

// overlayClosed is an overlay that is closed as soon as it is opened: what a
// stub returns until its task fills it in. The shell drops it unseen.
func overlayClosed() overlay { return overlayShut{} }

type overlayShut struct{}

func (o overlayShut) Update(tea.Msg) (overlay, tea.Cmd) { return o, nil }
func (overlayShut) View(int, int) string                { return "" }
func (overlayShut) Keys() []key.Binding                 { return nil }
func (overlayShut) Typing() bool                        { return false }
func (overlayShut) Closed() bool                        { return true }

// overlayNotYet is a step that says "not yet" and does nothing else: what a
// stub returns where a step is due, never a nil one.
func overlayNotYet() step { return overlayStub{} }

type overlayStub struct{}

func (overlayStub) Init() tea.Cmd                    { return nil }
func (s overlayStub) Update(tea.Msg) (step, tea.Cmd) { return s, nil }
func (overlayStub) View(int) string                  { return "not yet" }
func (overlayStub) Keys() []key.Binding              { return nil }
func (overlayStub) Typing() bool                     { return false }
