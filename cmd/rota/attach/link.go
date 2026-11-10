package attach

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// source is where an attach finds sessions and opens them: the rota daemon,
// through a *client.Client, or a test's script.
type source interface {
	// open attaches to the session id, watching only when watch is set, and
	// has read its hello. A daemon's no is a *client.Refusal.
	open(id string, watch bool) (link, error)
	// list is every session the daemon holds or lists, running and stopped.
	list() ([]client.Session, error)
}

// link is one attach to one session: a *client.Term, or a test's script.
// Next is one goroutine's; the rest may be called from any.
type link interface {
	Hello() client.Hello
	Next() (client.Frame, error)
	Type([]byte) error
	Fit(cols, rows uint16) error
	Claim(force bool) error
	Close() error
}

// linkClient is a source over the daemon's own client.
type linkClient struct{ c *client.Client }

func (s linkClient) open(id string, watch bool) (link, error) {
	t, err := s.c.Attach(id, watch)
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s linkClient) list() ([]client.Session, error) { return s.c.List() }

// linkRedialer is a source that a redial replaces: the client Options.Redial
// gave is the one the attach goes on with. A source that is not one, a
// test's, goes on as it was.
type linkRedialer interface {
	redial(c *client.Client) source
}

func (s linkClient) redial(c *client.Client) source {
	if c == nil {
		return s
	}
	return linkClient{c}
}

// linkCloseWait bounds how long the end of an attach waits for its links to
// say goodbye. A daemon that does not answer is left to notice the socket
// go.
var linkCloseWait = 200 * time.Millisecond

// linkWork runs the calls one link makes that wait on the daemon (typing,
// a fit, a claim, and last its close) on a goroutine of its own, one at a
// time and in the order they were asked, so what the person typed reaches
// the session in order while the attach's loop never waits: a paste into a
// CLI that is not reading stalls this goroutine, not the screen, a signal,
// the chord or another session's typing. Each link has its own, so a key
// meant for one session can never be sent on another's.
type linkWork struct {
	mu   sync.Mutex
	q    []func()
	wake chan struct{}
	quit chan struct{}
	once sync.Once
}

func linkNewWork() *linkWork {
	w := &linkWork{wake: make(chan struct{}, 1), quit: make(chan struct{})}
	go w.run()
	return w
}

// do queues f. It never waits.
func (w *linkWork) do(f func()) {
	w.mu.Lock()
	w.q = append(w.q, f)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *linkWork) run() {
	for {
		w.mu.Lock()
		if len(w.q) == 0 {
			w.mu.Unlock()
			select {
			case <-w.wake:
				continue
			case <-w.quit:
				return
			}
		}
		f := w.q[0]
		w.q[0] = nil
		w.q = w.q[1:]
		w.mu.Unlock()
		select {
		case <-w.quit:
			return
		default:
		}
		f()
	}
}

// stop abandons whatever is still queued. A call under way runs to its end.
func (w *linkWork) stop() {
	w.once.Do(func() { close(w.quit) })
}

// linkHeld is a link the attach opened, the queue of its calls, and
// whether it has been closed.
type linkHeld struct {
	l      link
	work   *linkWork
	closed atomic.Bool
}

func linkHold(l link) *linkHeld { return &linkHeld{l: l, work: linkNewWork()} }

func (h *linkHeld) close() {
	_ = h.l.Close()
	h.closed.Store(true)
}

// drop closes the link after every call already asked of it, and lets its
// queue go.
func (h *linkHeld) drop() {
	h.work.do(func() {
		h.close()
		h.work.stop()
	})
}
