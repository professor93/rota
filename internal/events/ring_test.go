package events

import (
	"sync"
	"testing"
	"time"
)

// clock is a settable time source for a Ring.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func TestIDsIncreaseAndFollowTheClock(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	c := &clock{t: at}
	r := NewRing(8, c.now)
	given := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for i := int64(0); i < 3; i++ {
		e, ok := r.Publish(Event{Type: Test, Time: given, Text: "x"})
		if !ok || e.ID != at.UnixMicro()+i {
			t.Fatalf("publish %d: id %d ok %v, want %d", i, e.ID, ok, at.UnixMicro()+i)
		}
		if !e.Time.Equal(given) || e.Text != "x" || e.Type != Test {
			t.Fatalf("Publish changed more than the id: %+v", e)
		}
	}
	c.set(at.Add(-time.Second))
	if e, _ := r.Publish(Event{Type: Test}); e.ID != at.UnixMicro()+3 {
		t.Fatalf("after the clock went back, id %d, want %d", e.ID, at.UnixMicro()+3)
	}
	if !(r.Last() == at.UnixMicro()+3) {
		t.Fatalf("Last = %d", r.Last())
	}
	later := at.Add(time.Hour)
	c.set(later)
	if e, _ := r.Publish(Event{Type: Test}); e.ID != later.UnixMicro() {
		t.Fatalf("after the clock went on, id %d, want %d", e.ID, later.UnixMicro())
	}
}

func TestARestartedRingKeepsRising(t *testing.T) {
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	c := &clock{t: at}
	old := NewRing(8, c.now)
	for range 5 {
		old.Publish(Event{Type: Test})
	}
	c.set(at.Add(time.Millisecond))
	fresh := NewRing(8, c.now)
	e, _ := fresh.Publish(Event{Type: Test})
	if e.ID <= old.Last() {
		t.Fatalf("the new ring's first id %d is not past the old one's last %d", e.ID, old.Last())
	}
	if evs, gap, _ := fresh.Since(old.Last()); gap || len(evs) != 1 {
		t.Fatalf("Since(old last) gave %d, gap %v", len(evs), gap)
	}
}

func TestSinceAndGap(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	r := NewRing(3, c.now)
	if evs, gap, _ := r.Since(0); len(evs) != 0 || gap {
		t.Fatalf("an empty ring gave %d, gap %v", len(evs), gap)
	}
	if r.Last() != 0 {
		t.Fatalf("an empty ring's Last is %d", r.Last())
	}
	var ids []int64
	for range 5 {
		e, _ := r.Publish(Event{Type: Test})
		ids = append(ids, e.ID)
	}
	evs, gap, _ := r.Since(0)
	if !gap || len(evs) != 3 || evs[0].ID != ids[2] || evs[2].ID != ids[4] {
		t.Fatalf("Since(0) gave %v, gap %v", evs, gap)
	}
	if evs, gap, _ := r.Since(-1); !gap || len(evs) != 3 {
		t.Fatalf("Since(-1) gave %d, gap %v", len(evs), gap)
	}
	evs, gap, _ = r.Since(ids[1])
	if gap || len(evs) != 3 || evs[0].ID != ids[2] {
		t.Fatalf("Since(second) gave %v, gap %v", evs, gap)
	}
	if evs, gap, _ := r.Since(ids[3]); gap || len(evs) != 1 || evs[0].ID != ids[4] {
		t.Fatalf("Since(fourth) gave %v, gap %v", evs, gap)
	}
	if evs, gap, _ := r.Since(r.Last()); gap || len(evs) != 0 {
		t.Fatalf("Since(Last) gave %v, gap %v", evs, gap)
	}
	evs[0].Text = "changed"
	if again, _, _ := r.Since(ids[3]); again[0].Text != "" {
		t.Fatal("Since handed out the ring's own storage")
	}
	if evs, _, _ := r.Since(r.Last() + 1000); len(evs) != 0 {
		t.Fatalf("Since(the future) gave %v", evs)
	}
}

func TestANewRingWithNoDropsHasNoGap(t *testing.T) {
	r := NewRing(4, nil)
	for range 4 {
		r.Publish(Event{Type: Test})
	}
	if evs, gap, _ := r.Since(0); gap || len(evs) != 4 {
		t.Fatalf("Since(0) on a full ring gave %d, gap %v", len(evs), gap)
	}
	if evs, gap, _ := NewRing(0, nil).Since(0); gap || len(evs) != 0 {
		t.Fatalf("a ring of 0 gave %d, gap %v", len(evs), gap)
	}
	one := NewRing(0, nil)
	one.Publish(Event{Type: Test})
	one.Publish(Event{Type: Test})
	if evs, _, _ := one.Since(0); len(evs) != 1 {
		t.Fatalf("a ring of 0 kept %d, want 1", len(evs))
	}
}

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

func TestWakeClosesOnPublishAndClose(t *testing.T) {
	r := NewRing(3, nil)
	_, _, wake := r.Since(r.Last())
	if closed(wake) {
		t.Fatal("wake was closed before any publish")
	}
	r.Publish(Event{Type: Test})
	if !closed(wake) {
		t.Fatal("wake was still open after a publish")
	}
	evs, _, fresh := r.Since(0)
	if len(evs) != 1 || closed(fresh) {
		t.Fatalf("after the publish: %d events, fresh wake closed %v", len(evs), closed(fresh))
	}
	r.Close()
	if !closed(fresh) {
		t.Fatal("wake was still open after Close")
	}
}

func TestAClosedRingTakesNothing(t *testing.T) {
	r := NewRing(3, nil)
	kept, _ := r.Publish(Event{Type: Test})
	if closed(r.Closed()) {
		t.Fatal("Closed() was closed before Close")
	}
	r.Close()
	r.Close() // twice is fine
	if !closed(r.Closed()) {
		t.Fatal("Closed() was open after Close")
	}
	if _, ok := r.Publish(Event{Type: Test}); ok {
		t.Fatal("a closed ring took an event")
	}
	if r.Last() != kept.ID {
		t.Fatalf("Last moved to %d after Close", r.Last())
	}
	evs, gap, wake := r.Since(0)
	if gap || len(evs) != 1 || evs[0].ID != kept.ID {
		t.Fatalf("Since(0) after Close gave %v, gap %v", evs, gap)
	}
	if !closed(wake) {
		t.Fatal("a closed ring handed out an open wake")
	}
}

// follow is the reader loop Since documents: read, and when there is
// nothing, wait for the wake or for Close; once closed, read once more, and
// end when that gives nothing. between runs after each read that gave
// nothing and before the wait, which is where a publish and a close can
// land together.
func follow(t *testing.T, r *Ring, between func()) []Event {
	var got []Event
	var last int64
	closed := false
	for {
		evs, _, wake := r.Since(last)
		for _, e := range evs {
			if e.ID <= last {
				t.Errorf("id %d after %d", e.ID, last)
				return got
			}
			last = e.ID
		}
		got = append(got, evs...)
		if len(evs) > 0 {
			continue
		}
		if closed {
			return got
		}
		if between != nil {
			between()
		}
		select {
		case <-r.Closed():
			closed = true
		case <-wake:
		}
	}
}

func TestTheLastEventBeforeCloseIsRead(t *testing.T) {
	// A publish and a close both landing while the reader is between its
	// read and its wait make both channels ready, and select picks either.
	// Without the read after Close, about half of these runs lose the event.
	for i := range 200 {
		r := NewRing(4, nil)
		once := false
		got := follow(t, r, func() {
			if !once {
				once = true
				r.Publish(Event{Type: DaemonStopped})
				r.Close()
			}
		})
		if len(got) != 1 || got[0].Type != DaemonStopped {
			t.Fatalf("run %d: the reader got %v, want daemon.stopped", i, got)
		}
	}
}

func TestPublishAndSinceRace(t *testing.T) {
	r := NewRing(16, nil)
	var writers, reader sync.WaitGroup
	for range 4 {
		writers.Go(func() {
			for range 200 {
				r.Publish(Event{Type: Test})
			}
		})
	}
	var got []Event
	reader.Go(func() { got = follow(t, r, nil) })
	writers.Wait()
	r.Close()
	reader.Wait()
	if len(got) == 0 || got[len(got)-1].ID != r.Last() {
		t.Fatalf("the reader ended short of the ring's last id %d", r.Last())
	}
}

func TestNoOneSharesAPointer(t *testing.T) {
	r := NewRing(4, nil)
	exit, pct := 0, 10.0
	r.Publish(Event{Type: SessionEnded, Exit: &exit, Percent: &pct})
	exit, pct = 1, 99
	a, _, _ := r.Since(0)
	if *a[0].Exit != 0 || *a[0].Percent != 10 {
		t.Fatalf("the publisher's change reached the ring: exit %d, percent %v", *a[0].Exit, *a[0].Percent)
	}
	*a[0].Exit, *a[0].Percent = 7, 70
	b, _, _ := r.Since(0)
	if *b[0].Exit != 0 || *b[0].Percent != 10 {
		t.Fatalf("one reader's change reached another: exit %d, percent %v", *b[0].Exit, *b[0].Percent)
	}
	if e, _ := r.Publish(Event{Type: Test}); e.Exit != nil || e.Percent != nil {
		t.Fatalf("an event without them gained them: %+v", e)
	}
}
