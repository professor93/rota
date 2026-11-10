package tui

import (
	"context"
	"sync"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/professor93/rota/api"
)

// Every Backend call carries a deadline (R29), so a daemon that hangs
// cannot hang the TUI. A login's finish may wait on a provider, so it has
// longer. An Ask has none: it ends when it is answered or stopped.
const (
	envCallTimeout   = 30 * time.Second
	envFinishTimeout = 60 * time.Second
)

// env is what the shell shares with every tab and overlay: the Backend, the
// options, the theme, and the latest of what the server said. The shell
// writes its fields in Update, before it hands the message on, and nothing
// else writes them. A command runs beside Update, so one that needs a field
// copies it before it returns the command, and reads none of them inside.
type env struct {
	b      Backend
	o      Options
	th     Theme
	who    Principal
	list   Listing
	daemon Daemon
	// width and height are the whole terminal's, as the last resize said.
	width, height int

	// ctx ends when the program does; every call runs under it.
	ctx context.Context
	win *envWindow
}

func (e *env) now() time.Time { return e.o.Now() }

func (e *env) tick(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
	return e.o.Tick(d, fn)
}

// control says whether the principal may change things. Until Who has
// answered nobody may, so a key pressed before it does is not shown as
// allowed (F29).
func (e *env) control() bool { return e.who.Role == string(api.RoleControl) }

// gate enables each binding for a control principal and disables it for
// anyone else. A tab gates its control-only bindings each time it builds
// its keys, so they follow the principal once Who answers.
func (e *env) gate(bs ...*key.Binding) {
	on := e.control()
	for _, b := range bs {
		b.SetEnabled(on)
	}
}

// call is a command that runs f under a deadline of d from now, and under
// the program's life, and returns what f returns. A d of zero or less sets
// no deadline but the program's.
func (e *env) call(d time.Duration, f func(ctx context.Context) tea.Msg) tea.Cmd {
	parent := e.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(parent)
		if d > 0 {
			ctx, cancel = context.WithTimeout(parent, d)
		}
		defer cancel()
		return f(ctx)
	}
}

// window is the terminal's size for an attach that holds it (Options.Attach):
// size answers the latest, resized has a value each time it changes, and
// release, called once the attach has ended, stops the signals.
func (e *env) window() (size func() (cols, rows int), resized <-chan struct{}, release func()) {
	ch, release := e.win.watch()
	return e.win.size, ch, release
}

// envWindow is the terminal's latest size, shared between the model, Run's
// reader of Options.Resize and an attach that holds the terminal, which
// asks for it from its own goroutine. While an attach runs, Bubble Tea
// calls no Update, so Run sets it directly.
type envWindow struct {
	mu         sync.Mutex
	cols, rows int
	subs       map[chan struct{}]struct{}
}

func (w *envWindow) size() (cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cols, w.rows
}

// set records a size and signals every watcher when it differs from the
// last. It never blocks: a watcher that has a signal waiting gets no second.
func (w *envWindow) set(cols, rows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if cols == w.cols && rows == w.rows {
		return
	}
	w.cols, w.rows = cols, rows
	for ch := range w.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (w *envWindow) watch() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.subs == nil {
		w.subs = map[chan struct{}]struct{}{}
	}
	w.subs[ch] = struct{}{}
	w.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.subs, ch)
			w.mu.Unlock()
		})
	}
}
