package tui

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// programLeave is how long the tabs have, after the program ends, to close
// what they hold open.
const programLeaveTimeout = 10 * time.Second

// Run runs the TUI on o's terminal until the person quits or ctx ends. It
// then leaves each tab, which closes what it holds open, and returns the
// login a flow left parked, if one did.
//
// Over SSH (Remote) the size comes from Cols and Rows, the colour depth
// from TERM, and no signal of this process is the TUI's. Run is the one
// reader of Resize there: each size goes to the model and, while an attach
// holds the terminal and Bubble Tea delivers nothing, to the attach.
func Run(ctx context.Context, o Options) (Result, error) {
	if o.Backend == nil {
		return Result{}, errors.New("tui: Options.Backend is required")
	}
	m := New(o)
	o = m.e.o
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.e.ctx = ctx
	opts := []tea.ProgramOption{
		tea.WithContext(ctx),
		tea.WithInput(o.In),
		tea.WithOutput(o.Out),
		tea.WithEnvironment(o.Env),
		tea.WithColorProfile(m.e.th.Profile),
	}
	if o.Remote {
		opts = append(opts, tea.WithWindowSize(o.Cols, o.Rows), tea.WithoutSignalHandler())
	}
	p := tea.NewProgram(m, opts...)
	var wg sync.WaitGroup
	if o.Resize != nil {
		programResize(ctx, &wg, p, m.e.win, o.Resize)
	}
	_, err := p.Run()
	cancel()
	wg.Wait()
	if lerr := programLeave(m.tabs); lerr != nil {
		err = errors.Join(err, lerr)
	}
	return m.result(), err
}

// programResize reads sizes until ctx ends. Each one is the window's at
// once, which an attach holding the terminal reads, and reaches the model
// as a resize when the program next takes a message; sizes that arrive
// meanwhile are coalesced into the latest.
func programResize(ctx context.Context, wg *sync.WaitGroup, p *tea.Program, win *envWindow, sizes <-chan [2]int) {
	kick := make(chan struct{}, 1)
	programGo(wg, p, "reading the terminal's size", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case s, ok := <-sizes:
				if !ok {
					return
				}
				win.set(s[0], s[1])
				select {
				case kick <- struct{}{}:
				default:
				}
			}
		}
	})
	programGo(wg, p, "resizing the screen", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
				c, r := win.size()
				p.Send(tea.WindowSizeMsg{Width: c, Height: r})
			}
		}
	})
}

// programGo runs f in a goroutine of its own that recovers: a panic there
// is said as a toast and ends only f, never the process, which over SSH is
// the daemon and every session it links (F31).
func programGo(wg *sync.WaitGroup, p *tea.Program, what string, f func()) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				p.Send(toastMsg{Text: fmt.Sprintf("%s failed: %v", what, r), Level: levelErr})
			}
		}()
		f()
	}()
}

// programLeave leaves each tab under one deadline. A tab that panics is
// said in the error, and the others are still left.
func programLeave(tabs []tab) error {
	ctx, cancel := context.WithTimeout(context.Background(), programLeaveTimeout)
	defer cancel()
	var errs []error
	for _, t := range tabs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					errs = append(errs, fmt.Errorf("tui: leaving %s: %v\n%s", t.Title(), r, debug.Stack()))
				}
			}()
			t.Leave(ctx)
		}()
	}
	return errors.Join(errs...)
}
