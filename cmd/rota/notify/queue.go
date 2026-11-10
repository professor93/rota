package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/professor93/rota/internal/events"
)

// stopping is why an event was dropped when Wait gave up on it.
const stopping = "the daemon was stopping"

// sink is one sink at work: its queue, the event it is sending, and how it
// stands. One goroutine sends, in order; the reader only queues.
type sink struct {
	n    *Notifier
	snd  sender // nil: it could not be made, and is sent nothing
	keys []string
	on   []string
	wake chan struct{}

	mu       sync.Mutex
	queue    []events.Event
	busy     bool // flight is being sent
	flight   events.Event
	final    bool // Wait has counted what was left; nothing later counts
	st       Status
	drops    int // dropped since the log last heard of it
	dropWhy  string
	dropSaid time.Time
}

// put queues e, letting the oldest event go when the queue is full. A sink
// that could not be made takes nothing, and answers a test with why.
func (s *sink) put(e events.Event) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	switch {
	case s.snd == nil:
		if e.Type == events.Test {
			s.st.LastTest = &TestResult{ID: e.ID, Error: s.st.LastError, At: now}
			s.n.dirty.Store(true)
		}
		return nil
	case s.final:
		return s.dropLocked(e, stopping, now)
	}
	var lines []line
	if len(s.queue) >= s.n.k.depth {
		lines = s.dropLocked(s.queue[0], "its queue was full", now)
		s.queue = slices.Delete(s.queue, 0, 1)
	}
	s.queue = append(s.queue, e)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return lines
}

// work sends what is queued until the reader is done and the queue is
// empty, or until Wait gives up.
func (s *sink) work() {
	for {
		e, ok := s.next()
		if !ok {
			return
		}
		s.deliver(e)
	}
}

// next takes the oldest queued event, waiting for one, and is false once
// there will be none.
func (s *sink) next() (events.Event, bool) {
	for {
		s.mu.Lock()
		if s.final {
			s.mu.Unlock()
			return events.Event{}, false
		}
		if len(s.queue) > 0 {
			e := s.queue[0]
			s.queue = slices.Delete(s.queue, 0, 1)
			s.busy, s.flight = true, e
			s.mu.Unlock()
			return e, true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.n.read:
			// The reader queues nothing after it is done, so an empty
			// queue now stays empty.
			s.mu.Lock()
			empty := len(s.queue) == 0
			s.mu.Unlock()
			if empty {
				return events.Event{}, false
			}
		case <-s.n.ctx.Done():
			return events.Event{}, false
		}
	}
}

// deliver sends e, trying again after each of backoff's waits, or after the
// sink's own when that is longer, up to maxAfter. A test is tried once: it
// is a question somebody is waiting on the answer to.
func (s *sink) deliver(e events.Event) {
	k := s.n.k
	for try := 0; ; try++ {
		if !e.Time.IsZero() && time.Since(e.Time) > k.maxAge {
			s.n.say(s.done(e, nil, "older than "+span(k.maxAge)))
			return
		}
		ctx, cancel := context.WithTimeout(s.n.ctx, k.attempt)
		err := s.snd.send(ctx, e)
		cancel()
		if err == nil {
			s.n.say(s.sent(e))
			return
		}
		if s.n.ctx.Err() != nil {
			s.n.say(s.done(e, nil, stopping)) // not the sink's failure
			return
		}
		msg := cleanErr(err, s.keys)
		var d *drop
		switch {
		case errors.As(err, &d) || e.Type == events.Test:
			s.n.say(s.done(e, &msg, msg))
			return
		case try >= len(k.backoff):
			why := msg
			if try > 0 {
				why = fmt.Sprintf("%s (tried %d times)", msg, try+1)
			}
			s.n.say(s.done(e, &msg, why))
			return
		}
		s.n.say(s.failed(msg))
		wait := k.backoff[try]
		var r *retry
		if errors.As(err, &r) && r.after > wait {
			wait = max(wait, min(r.after, k.maxAfter))
		}
		if !s.n.sleep(wait) {
			s.n.say(s.done(e, nil, stopping))
			return
		}
	}
}

// sent records the event in flight as delivered.
func (s *sink) sent(e events.Event) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	now := time.Now().UTC()
	s.busy = false
	s.st.Sent++
	s.st.LastSent = now
	if e.Type == events.Test {
		s.st.LastTest = &TestResult{ID: e.ID, OK: true, At: now}
	}
	s.n.dirty.Store(true)
	was := s.st.State
	s.st.State = stateOK
	if was == stateFailing {
		return []line{{false, fmt.Sprintf("events: %s sends again", s.st.Name)}}
	}
	return nil
}

// failed records an attempt that failed with msg and will be tried again.
func (s *sink) failed(msg string) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	return s.failLocked(msg, time.Now().UTC())
}

// done records the event in flight as dropped for why, after an attempt
// that failed with msg when there was one.
func (s *sink) done(e events.Event, msg *string, why string) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	now := time.Now().UTC()
	s.busy = false
	var lines []line
	if msg != nil {
		lines = s.failLocked(*msg, now)
	}
	return append(lines, s.dropLocked(e, why, now)...)
}

// failLocked makes the sink failing with msg, said when it starts to be.
func (s *sink) failLocked(msg string, now time.Time) []line {
	s.st.LastError, s.st.LastErrorAt = msg, now
	s.n.dirty.Store(true)
	if s.st.State == stateFailing {
		return nil
	}
	s.st.State = stateFailing
	return []line{{true, fmt.Sprintf("events: %s failing: %s", s.st.Name, msg)}}
}

// dropLocked counts e as dropped for why, and answers it when it is a test.
func (s *sink) dropLocked(e events.Event, why string, now time.Time) []line {
	s.st.Dropped++
	if e.Type == events.Test {
		s.st.LastTest = &TestResult{ID: e.ID, Error: why, At: now}
	}
	s.drops++
	s.dropWhy = why
	s.n.dirty.Store(true)
	return s.sayDropsLocked(now, false)
}

// sayDropsLocked is the line about drops not said yet: at once for the
// first, then at most once each dropSayEvery, with the latest why; or now
// whatever the time when all is true.
func (s *sink) sayDropsLocked(now time.Time, all bool) []line {
	if s.drops == 0 || (!all && !s.dropSaid.IsZero() && now.Sub(s.dropSaid) < s.n.k.dropSay) {
		return nil
	}
	l := line{true, fmt.Sprintf("events: %s dropped %s: %s", s.st.Name, plural(s.drops, "event"), s.dropWhy)}
	s.drops, s.dropSaid = 0, now
	return []line{l}
}

// finish gives up on the sink: what is queued, and the event in flight,
// count as dropped, and nothing it does after counts.
func (s *sink) finish(now time.Time) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	s.final = true
	left := s.queue
	s.queue = nil
	if s.busy {
		left = append([]events.Event{s.flight}, left...)
		s.busy = false
	}
	var lines []line
	for _, e := range left {
		lines = append(lines, s.dropLocked(e, stopping, now)...)
	}
	return lines
}

// plural is n and word, with an s unless n is 1.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// span is d as a person says it: whole minutes in words, else Go's way.
func span(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return plural(int(d/time.Minute), "minute")
	}
	return d.String()
}
