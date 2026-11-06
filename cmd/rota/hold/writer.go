//go:build linux || darwin

package hold

import (
	"fmt"
	"io"
	"runtime/debug"
	"sync"
)

// maxReplies bounds the answers waiting for a CLI that asks and does not
// read. A terminal query is a few bytes and its answer a few dozen, so a CLI
// printing queries without reading its input would otherwise grow the holder
// for as long as it ran. Past the bound answers are dropped, said once.
const maxReplies = 1 << 20

// writer is the only thing that writes to the CLI.
//
// Two queues feed it. Answers to the CLI's own queries go first and are
// never refused for room the keystrokes take: a CLI waiting for an answer
// waits behind nobody's typing. Keystrokes queue behind them, up to
// InputQueue bytes, counted until they are written; past that a keystroke
// is refused and the daemon is told, so the person who typed it learns it
// was not typed rather than finding it missing.
//
// Each item is written whole, so an answer never lands in the middle of a
// paste or a key's escape sequence.
type writer struct {
	child Child
	max   int
	log   func(msg string, args ...any)

	mu      sync.Mutex
	replies [][]byte
	held    int // bytes in replies
	input   []*typing
	queued  int // keystroke bytes not yet written
	dropped bool
	dead    bool
	wake    chan struct{}
	quit    chan struct{}
	once    sync.Once
}

// typing is keystrokes on their way, and a channel closed once they are
// written.
type typing struct {
	p       []byte
	counted bool
	done    chan struct{}
}

func newWriter(c Child, max int, log func(msg string, args ...any)) *writer {
	return &writer{child: c, max: max, log: log, wake: make(chan struct{}, 1), quit: make(chan struct{})}
}

// reply queues answers ahead of every keystroke.
func (w *writer) reply(rs ...[]byte) {
	if len(rs) == 0 {
		return
	}
	w.mu.Lock()
	for _, r := range rs {
		if w.held+len(r) > maxReplies {
			if !w.dropped {
				w.dropped = true
				w.log("the CLI asks its terminal faster than it reads the answers; answers are being dropped")
			}
			continue
		}
		w.replies = append(w.replies, r)
		w.held += len(r)
	}
	w.mu.Unlock()
	nudge(w.wake)
}

// typeIn queues keystrokes, and says false when there is no room for them.
// Once the CLI is gone there is nothing to type into, and nothing to refuse.
func (w *writer) typeIn(p []byte) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return true
	}
	if w.queued+len(p) > w.max {
		return false
	}
	w.queued += len(p)
	w.input = append(w.input, &typing{p: append([]byte(nil), p...), counted: true, done: make(chan struct{})})
	nudge(w.wake)
	return true
}

// paste queues text a send types, whatever the queue holds: a send is one
// frame at most and one at a time. The channel closes once it is written.
func (w *writer) paste(p []byte) <-chan struct{} {
	t := &typing{p: p, done: make(chan struct{})}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.dead {
		w.input = append(w.input, t)
		nudge(w.wake)
	}
	return t.done
}

// run writes until the CLI's terminal refuses a write or stop is called.
// A panic costs the item being written, and is logged.
func (w *writer) run() {
	for {
		p, t, ok := w.next()
		if !ok {
			return
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					w.log("a panic in the writer", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
				}
			}()
			err := writeAll(w.child, p)
			if t != nil {
				w.mu.Lock()
				if t.counted {
					w.queued -= len(p)
				}
				w.mu.Unlock()
				close(t.done)
			}
			if err != nil {
				w.log("writing to the CLI failed; nothing more is typed into it", "err", err)
				w.stop()
			}
		}()
	}
}

// next is the next thing to write, answers first, waiting until there is
// one. False means the writer has stopped.
func (w *writer) next() ([]byte, *typing, bool) {
	for {
		w.mu.Lock()
		switch {
		case w.dead:
			w.mu.Unlock()
			return nil, nil, false
		case len(w.replies) > 0:
			p := w.replies[0]
			w.replies[0] = nil
			w.replies = w.replies[1:]
			if w.held -= len(p); w.held == 0 {
				w.dropped = false
			}
			w.mu.Unlock()
			return p, nil, true
		case len(w.input) > 0:
			t := w.input[0]
			w.input[0] = nil
			w.input = w.input[1:]
			w.mu.Unlock()
			return t.p, t, true
		}
		w.mu.Unlock()
		select {
		case <-w.wake:
		case <-w.quit:
		}
	}
}

// stop ends run, and with it every write still queued.
func (w *writer) stop() {
	w.mu.Lock()
	w.dead = true
	w.mu.Unlock()
	w.once.Do(func() { close(w.quit) })
}

// writeAll writes p whole, in as many writes as the terminal takes.
func writeAll(c Child, p []byte) error {
	for len(p) > 0 {
		n, err := c.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
