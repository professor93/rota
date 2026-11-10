package events

import (
	"sort"
	"sync"
	"time"
)

// Ring keeps the newest events a server has published, gives each its id,
// and wakes whoever waits for the next one. It is bounded: once full, each
// publish pushes the oldest event out, and a reader asking from before that
// event is told there is a gap.
//
// An id is max(now in microseconds since the Unix epoch, the last id + 1).
// So ids rise strictly, and keep rising across a restart: a client's
// Last-Event-ID from before the daemon restarted is older than anything the
// new daemon publishes, so it gets the new ring's whole contents and nothing
// twice. A wall clock stepped back keeps the ids rising by one each until it
// catches up. An id past the newest this ring gave (a daemon whose clock ran
// ahead, or a stream from another ring) is not trusted: Since gives
// everything kept, after a gap.
//
// A Ring is safe for concurrent use.
type Ring struct {
	now func() time.Time

	mu      sync.Mutex
	buf     []Event // used circularly once it is full
	start   int     // the oldest event's index, once buf is full
	keep    int
	last    int64         // the newest id given
	dropped int64         // the newest id pushed out, 0 for none
	wake    chan struct{} // closed on the next publish, or on Close
	closed  chan struct{}
	done    bool
}

// NewRing keeps the newest keep events, and at least one. now is the
// clock the ids follow; nil is time.Now.
func NewRing(keep int, now func() time.Time) *Ring {
	if keep < 1 {
		keep = 1
	}
	if now == nil {
		now = time.Now
	}
	return &Ring{
		now:    now,
		buf:    make([]Event, 0, min(keep, 64)),
		keep:   keep,
		wake:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

// Publish gives e its id, keeps it and wakes every reader. It changes
// nothing else about e: its Time is what the caller gave. Exit and Percent
// are copied, so the caller may change what it pointed them at. It returns
// the event as kept, and true; once the ring is closed it keeps nothing and
// returns e as given, and false.
func (r *Ring) Publish(e Event) (Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return e, false
	}
	e = own(e)
	e.ID = max(r.now().UnixMicro(), r.last+1)
	r.last = e.ID
	if len(r.buf) < r.keep {
		r.buf = append(r.buf, e)
	} else {
		r.dropped = r.buf[r.start].ID
		r.buf[r.start] = e
		r.start = (r.start + 1) % r.keep
	}
	close(r.wake)
	r.wake = make(chan struct{})
	return own(e), true
}

// own is e with Exit and Percent pointing at values of its own, so no two
// holders of an event share either.
func own(e Event) Event {
	if e.Exit != nil {
		v := *e.Exit
		e.Exit = &v
	}
	if e.Percent != nil {
		v := *e.Percent
		e.Percent = &v
	}
	return e
}

// Since is every kept event whose id is after id, oldest first. The slice
// and the events in it are the caller's own, Exit and Percent included.
// gap is true when events after id may be lost, and the caller says so
// (GapFrame) before the rest. That is so in two cases:
//
//   - events after id were pushed out before they could be read;
//   - id is past Last(), so this ring never gave it: a daemon whose clock
//     ran ahead, or a stream from another ring. Since then gives everything
//     kept.
//
// gap is never true with no events, so evs[0].ID is there for GapFrame. An
// id from before this ring was made, such as a reconnect across a restart,
// gives everything kept and no gap. An id below 0 is taken as 0, which asks
// for everything kept.
//
// wake is closed by the next Publish, or by Close, so a reader that has
// everything waits on it and then asks again. A Publish and a Close can
// land together while a reader is between Since and its wait, and then
// both wake and Closed() are ready. So a reader that sees Closed() closed
// calls Since once more, and is done only when that call gives nothing:
//
//	closed := false
//	for {
//		evs, gap, wake := r.Since(last)
//		// say the gap, send evs, and move last to the newest
//		if len(evs) > 0 {
//			continue
//		}
//		if closed {
//			return // every event the ring took has been read
//		}
//		select {
//		case <-r.Closed():
//			closed = true
//		case <-wake:
//		}
//	}
//
// Once the ring is closed, wake is already closed.
func (r *Ring) Since(id int64) (evs []Event, gap bool, wake <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id = max(id, 0)
	n := len(r.buf)
	gap = id < r.dropped
	if id > r.last && n > 0 {
		id, gap = 0, true // never given here: everything, after a gap
	}
	at := func(i int) Event { return r.buf[(r.start+i)%n] }
	first := sort.Search(n, func(i int) bool { return at(i).ID > id })
	if first < n {
		evs = make([]Event, 0, n-first)
		for i := first; i < n; i++ {
			evs = append(evs, own(at(i)))
		}
	}
	return evs, gap, r.wake
}

// Last is the newest id the ring has given, 0 before the first. A stream
// asked for events "from now" starts after it.
func (r *Ring) Last() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Close stops the ring taking events and wakes every reader for the last
// time. What it kept can still be read. Closing twice is fine.
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	close(r.wake) // left closed: nothing will publish again
	close(r.closed)
}

// Closed is closed once Close has been called.
func (r *Ring) Closed() <-chan struct{} { return r.closed }
