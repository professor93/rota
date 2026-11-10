package tui

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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
	// envRefreshTimeout is a listing read that refreshes usage (stale or
	// forced). The server first takes a run slot for it, which can wait
	// while Asks run, and then refreshes under the store's RefreshTimeout,
	// 30 s plus a 5 s margin; 30 s would cut a refresh that is going well.
	envRefreshTimeout = 60 * time.Second
	// envCallGrace is how long a call waits, past its deadline, for a callee
	// that has not noticed it, before it answers for the callee.
	envCallGrace = time.Second
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
// no deadline but the program's. A failure f cannot answer for itself, a
// panic or a deadline it ignored, is a levelErr toast and nothing else: a
// caller that keeps the call marked in flight until its own message comes
// uses callOr, or that mark never clears.
func (e *env) call(d time.Duration, f func(ctx context.Context) tea.Msg) tea.Cmd {
	return e.callOr(d, f, func(err error) tea.Msg {
		return toastMsg{Text: err.Error(), Level: levelErr}
	}, nil)
}

// callOr is call, with failed making the message for a call that panicked,
// or that had not returned envCallGrace after its context ended. Such a
// callee is left to finish by itself and what it returns goes nowhere, so
// a kind of call kept in flight until its answer comes is never stuck there.
//
// parked, when not nil, counts the callees of one kind answered for and
// still running: one more when callOr answers for it, one fewer when it
// does return. A caller that sends that kind again only at zero leaves at
// most one such callee running, however long it takes.
//
// A panic is said in one line, with no stack: the TUI has no log to put
// one in yet.
func (e *env) callOr(d time.Duration, f func(ctx context.Context) tea.Msg, failed func(error) tea.Msg, parked *atomic.Int32) tea.Cmd {
	parent := e.ctx
	return func() tea.Msg {
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)
		if d > 0 {
			ctx, cancel = context.WithTimeout(parent, d)
		} else {
			ctx, cancel = context.WithCancel(parent)
		}
		defer cancel()
		// state is envRunning until f returns (envReturned) or callOr
		// answers for it (envAnswered), whichever comes first.
		var state atomic.Int32
		got := make(chan tea.Msg, 1)
		go func() {
			var msg tea.Msg
			defer func() {
				if r := recover(); r != nil {
					msg = failed(fmt.Errorf("the call failed: %v", r))
				}
				if !state.CompareAndSwap(envRunning, envReturned) && parked != nil {
					parked.Add(-1)
				}
				got <- msg
			}()
			msg = f(ctx)
		}()
		select {
		case msg := <-got:
			return msg
		case <-ctx.Done():
		}
		grace := time.NewTimer(envCallGrace)
		defer grace.Stop()
		select {
		case msg := <-got:
			return msg
		case <-grace.C:
		}
		if parked != nil {
			parked.Add(1)
		}
		if !state.CompareAndSwap(envRunning, envAnswered) {
			// f returned just now: its own answer stands.
			if parked != nil {
				parked.Add(-1)
			}
			return <-got
		}
		return failed(fmt.Errorf("no answer: %w", ctx.Err()))
	}
}

// What callOr knows of its callee.
const (
	envRunning int32 = iota
	envReturned
	envAnswered
)

// window is the terminal's size for an attach that holds it (Options.Attach):
// size answers the latest, resized has a value each time it changes, and
// release, called once the attach has ended, stops the signals. Both follow
// Options.Resize, which Run reads even while Bubble Tea is held. Locally
// there is none: they hold the size at the hand-off and never signal, and a
// local attach watches its own terminal.
func (e *env) window() (size func() (cols, rows int), resized <-chan struct{}, release func()) {
	ch, release := e.win.watch()
	return e.win.size, ch, release
}

// envWindow is the terminal's latest size, shared between the model, Run's
// reader of Options.Resize and an attach that holds the terminal, which
// asks for it from its own goroutine. With Options.Resize, Run alone sets
// it, as each size arrives, since Bubble Tea calls no Update while an attach
// runs and may deliver its own first size after a later one. Without, the
// model sets it on each resize.
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
