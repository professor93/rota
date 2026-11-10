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

// stopping is why an event was dropped when the daemon stopped before it
// could be sent or tried again.
const stopping = "the daemon was stopping"

// sink is one sink at work: its queue, the event it is sending, and how it
// stands. One goroutine sends; the reader only queues.
//
// Events are sent in the order they came. A test is not one of them: it is
// a question somebody is waiting on, so it goes before the queue, and is
// sent even while an event waits to be tried again (poke).
type sink struct {
	n    *Notifier
	snd  sender // nil: it could not be made, and is sent nothing
	keys []string
	on   []string
	wake chan struct{} // something was queued
	poke chan struct{} // a test was queued

	mu      sync.Mutex
	queue   []events.Event
	tests   []events.Event
	busy    bool // flight is being sent, or waits to be tried again
	flight  events.Event
	probing bool // probe, a test, is being sent while flight waits
	probe   events.Event
	final   bool // Wait has counted what was left; nothing later counts
	st      Status
	drops   int // dropped since the log last heard of it
	dropWhy string
	said    time.Time
}

// put queues e, letting the oldest of its kind go when that queue is full.
// A sink that could not be made takes nothing, and answers a test with why.
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
		s.countLocked(e, stopping, now) // sinks.json has been written for the last time
		return nil
	}
	q := &s.queue
	if e.Type == events.Test {
		q = &s.tests
	}
	var lines []line
	if len(*q) >= s.n.k.depth {
		lines = s.dropLocked((*q)[0], "its queue was full", now)
		*q = slices.Delete(*q, 0, 1)
	}
	*q = append(*q, e)
	signal(s.wake)
	if e.Type == events.Test {
		signal(s.poke)
	}
	return lines
}

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// work sends what is queued until the reader is done and nothing is left,
// or until Wait gives up.
func (s *sink) work() {
	for {
		e, ok := s.next()
		if !ok {
			return
		}
		if e.Type == events.Test {
			s.n.say(s.once(e, false))
			continue
		}
		s.deliver(e)
	}
}

// next takes what to send next, a waiting test first, waiting for one; it
// is false once there will be nothing.
func (s *sink) next() (events.Event, bool) {
	for {
		s.mu.Lock()
		if s.final || s.n.ctx.Err() != nil {
			s.mu.Unlock()
			return events.Event{}, false
		}
		if e, ok := s.takeLocked(); ok {
			s.busy, s.flight = true, e
			s.mu.Unlock()
			return e, true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.n.read:
			// The reader queues nothing after it is done, so a sink with
			// nothing now will have nothing.
			s.mu.Lock()
			empty := len(s.queue) == 0 && len(s.tests) == 0
			s.mu.Unlock()
			if empty {
				return events.Event{}, false
			}
		case <-s.n.ctx.Done():
			return events.Event{}, false
		}
	}
}

// takeLocked takes the oldest waiting test, else the oldest queued event.
func (s *sink) takeLocked() (events.Event, bool) {
	for _, q := range []*[]events.Event{&s.tests, &s.queue} {
		if len(*q) > 0 {
			e := (*q)[0]
			*q = slices.Delete(*q, 0, 1)
			return e, true
		}
	}
	return events.Event{}, false
}

// deliver sends e, trying again after each of backoff's waits, or after the
// sink's own when that is longer, up to maxAfter. Once the daemon is
// stopping it is not tried again: pause ends at once.
func (s *sink) deliver(e events.Event) {
	k := s.n.k
	for try := 0; ; try++ {
		if s.old(e) {
			s.n.say(s.done(e, false, nil, "older than "+span(k.maxAge)))
			return
		}
		err := s.attempt(e)
		if err == nil {
			s.n.say(s.sent(e, false))
			return
		}
		if s.n.ctx.Err() != nil {
			s.n.say(s.done(e, false, nil, stopping)) // not the sink's failure
			return
		}
		msg := cleanErr(err, s.keys)
		var d *drop
		switch {
		case errors.As(err, &d):
			s.n.say(s.done(e, false, &msg, msg))
			return
		case try >= len(k.backoff):
			why := msg
			if try > 0 {
				why = fmt.Sprintf("%s (tried %d times)", msg, try+1)
			}
			s.n.say(s.done(e, false, &msg, why))
			return
		}
		s.n.say(s.failed(msg))
		wait := k.backoff[try]
		var r *retry
		if errors.As(err, &r) && r.after > wait {
			wait = max(wait, min(r.after, k.maxAfter))
		}
		if !s.pause(wait) {
			s.n.say(s.done(e, false, nil, stopping))
			return
		}
	}
}

// once is a test's one attempt. probe says it is sent while another event
// waits to be tried again.
func (s *sink) once(e events.Event, probe bool) []line {
	if s.old(e) {
		return s.done(e, probe, nil, "older than "+span(s.n.k.maxAge))
	}
	err := s.attempt(e)
	switch {
	case err == nil:
		return s.sent(e, probe)
	case s.n.ctx.Err() != nil:
		return s.done(e, probe, nil, stopping)
	}
	msg := cleanErr(err, s.keys)
	return s.done(e, probe, &msg, msg)
}

// attempt is one try at sending e, bounded by attemptTimeout.
func (s *sink) attempt(e events.Event) error {
	ctx, cancel := context.WithTimeout(s.n.ctx, s.n.k.attempt)
	defer cancel()
	return s.snd.send(ctx, e)
}

// old reports whether e is too old to be sent. An event with no time is
// never.
func (s *sink) old(e events.Event) bool {
	return !e.Time.IsZero() && time.Since(e.Time) > s.n.k.maxAge
}

// pause waits d before an event is tried again, sending each test that
// comes meanwhile, and is false when the daemon stops first.
func (s *sink) pause(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			return true
		case <-s.n.stop:
			return false
		case <-s.n.ctx.Done():
			return false
		case <-s.poke:
			s.probeTests()
		}
	}
}

// probeTests sends each waiting test once, while the event in flight waits.
func (s *sink) probeTests() {
	for {
		s.mu.Lock()
		if s.final || s.n.ctx.Err() != nil || len(s.tests) == 0 {
			s.mu.Unlock()
			return
		}
		e := s.tests[0]
		s.tests = slices.Delete(s.tests, 0, 1)
		s.probing, s.probe = true, e
		s.mu.Unlock()
		s.n.say(s.once(e, true))
	}
}

// settle is the end of the event in flight, or of the probe when probe.
func (s *sink) settle(probe bool) {
	if probe {
		s.probing = false
	} else {
		s.busy = false
	}
}

// sent records e, the event in flight or the probe, as delivered.
func (s *sink) sent(e events.Event, probe bool) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	now := time.Now().UTC()
	s.settle(probe)
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

// done records e, the event in flight or the probe, as dropped for why,
// after an attempt that failed with msg when there was one.
func (s *sink) done(e events.Event, probe bool, msg *string, why string) []line {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return nil
	}
	now := time.Now().UTC()
	s.settle(probe)
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

// dropLocked counts e as dropped for why, and says so by sayDropsLocked's
// rule.
func (s *sink) dropLocked(e events.Event, why string, now time.Time) []line {
	s.countLocked(e, why, now)
	return s.sayDropsLocked(now, false)
}

// countLocked counts e as dropped for why, answering it when it is a test,
// without saying anything.
func (s *sink) countLocked(e events.Event, why string, now time.Time) {
	s.st.Dropped++
	if e.Type == events.Test {
		s.st.LastTest = &TestResult{ID: e.ID, Error: why, At: now}
	}
	s.drops++
	s.dropWhy = why
	s.n.dirty.Store(true)
}

// sayDropsLocked is the line about drops not said yet: at once for the
// first, then at most once each dropSayEvery, with the latest why; or now
// whatever the time when all is true.
func (s *sink) sayDropsLocked(now time.Time, all bool) []line {
	if s.drops == 0 || (!all && !s.said.IsZero() && now.Sub(s.said) < s.n.k.dropSay) {
		return nil
	}
	l := line{true, fmt.Sprintf("events: %s dropped %s: %s", s.st.Name, plural(s.drops, "event"), s.dropWhy)}
	s.drops, s.said = 0, now
	return []line{l}
}

// finish gives up on the sink: what waits, and what is in flight, count as
// dropped, said in one line when Wait says the drops, and nothing the sink
// does after counts.
func (s *sink) finish(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.final {
		return
	}
	s.final = true
	var left []events.Event
	if s.busy {
		left = append(left, s.flight)
	}
	if s.probing {
		left = append(left, s.probe)
	}
	left = append(append(left, s.tests...), s.queue...)
	s.busy, s.probing, s.tests, s.queue = false, false, nil, nil
	for _, e := range left {
		s.countLocked(e, stopping, now)
	}
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
