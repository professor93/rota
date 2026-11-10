package notify

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/internal/events"
)

// A sink that never answers holds up no other (Review Focus 1).
func TestASlowSinkDelaysNobodyElse(t *testing.T) {
	quick(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	a := &fakeSender{answer: func(context.Context, int, events.Event) error { <-stuck; return nil }}
	b := &fakeSender{}
	useSenders(t, map[string]any{"telegram": a, "webhook": b})
	src := newSource()
	d := &dialer{plan: []any{src}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, webhookSink}}, Dial: d.dial})

	began := time.Now()
	for i := int64(1); i <= 10; i++ {
		src.feed <- ev(i, events.SessionAttention)
	}
	for len(b.delivered()) < 10 {
		if time.Since(began) > 200*time.Millisecond {
			t.Fatalf("the webhook had %d of ten events after 200ms", len(b.delivered()))
		}
		time.Sleep(time.Millisecond)
	}
	if a.tries() != 1 {
		t.Fatalf("the stuck sink was tried %d times", a.tries())
	}
}

// A failed attempt is tried again after each wait of backoff, then dropped;
// a sink that says to drop is not tried again; and a sink that says when to
// try again is waited for.
func TestRetriesBackOffThenDrop(t *testing.T) {
	busy := errors.New("the chat is busy")
	run := func(t *testing.T, f *fakeSender, e events.Event) (*Notifier, *logBuffer) {
		t.Helper()
		quick(t)
		useSenders(t, map[string]any{"webhook": f})
		log, buf := newLog()
		d := &dialer{plan: []any{newSource(e)}}
		return startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log}), buf
	}

	t.Run("three failures then a success", func(t *testing.T) {
		f := &fakeSender{answer: func(_ context.Context, call int, _ events.Event) error {
			if call <= 3 {
				return again(busy, 0)
			}
			return nil
		}}
		n, buf := run(t, f, ev(1, events.AccountDead))
		settled(t, "the log to say it sends again", func() bool { return buf.count("events: webhook sends again") == 1 })
		s := statusOf(t, n, "webhook")
		if f.tries() != 4 || len(f.delivered()) != 1 || s.Sent != 1 || s.Dropped != 0 || s.State != stateOK || s.LastSent.IsZero() {
			t.Fatalf("tried %d times, delivered %d: %+v", f.tries(), len(f.delivered()), s)
		}
		if s.LastError != "the chat is busy" || s.LastErrorAt.IsZero() {
			t.Fatalf("the last error is %q at %v", s.LastError, s.LastErrorAt)
		}
		if buf.count("events: webhook failing: the chat is busy") != 1 {
			t.Fatalf("the log: %s", buf)
		}
	})

	t.Run("always failing", func(t *testing.T) {
		f := &fakeSender{answer: func(context.Context, int, events.Event) error { return again(busy, 0) }}
		n, buf := run(t, f, ev(1, events.AccountDead))
		settled(t, "the log to say the drop", func() bool {
			return buf.count("events: webhook dropped 1 event: the chat is busy (tried 7 times)") == 1
		})
		s := statusOf(t, n, "webhook")
		if f.tries() != 7 || s.Sent != 0 || s.Dropped != 1 || s.State != stateFailing {
			t.Fatalf("tried %d times: %+v", f.tries(), s)
		}
		if buf.count("failing:") != 1 {
			t.Fatalf("the log: %s", buf)
		}
	})

	t.Run("dropped is not retried", func(t *testing.T) {
		f := &fakeSender{answer: func(context.Context, int, events.Event) error { return dropped(errors.New("no such chat")) }}
		n, buf := run(t, f, ev(1, events.AccountDead))
		settled(t, "the log to say the drop", func() bool { return buf.count("events: webhook dropped 1 event: no such chat") == 1 })
		if f.tries() != 1 || statusOf(t, n, "webhook").Dropped != 1 {
			t.Fatalf("tried %d times; the log: %s", f.tries(), buf)
		}
	})

	t.Run("an error that says nothing is retried", func(t *testing.T) {
		f := &fakeSender{answer: func(_ context.Context, call int, _ events.Event) error {
			if call == 1 {
				return busy
			}
			return nil
		}}
		n, _ := run(t, f, ev(1, events.AccountDead))
		eventually(t, "the event to be sent", func() bool { return statusOf(t, n, "webhook").Sent == 1 })
		if f.tries() != 2 {
			t.Fatalf("tried %d times", f.tries())
		}
	})

	t.Run("again after a while waits for it", func(t *testing.T) {
		f := &fakeSender{answer: func(_ context.Context, call int, _ events.Event) error {
			if call == 1 {
				return again(busy, 50*time.Millisecond)
			}
			return nil
		}}
		n, _ := run(t, f, ev(1, events.AccountDead))
		eventually(t, "the event to be sent", func() bool { return statusOf(t, n, "webhook").Sent == 1 })
		at := f.times()
		if len(at) != 2 || at[1].Sub(at[0]) < 50*time.Millisecond {
			t.Fatalf("the attempts came at %v", at)
		}
	})

	t.Run("a test event is tried once", func(t *testing.T) {
		f := &fakeSender{answer: func(context.Context, int, events.Event) error { return again(busy, 0) }}
		n, _ := run(t, f, ev(9, events.Test))
		settled(t, "the test to be answered once", func() bool { return statusOf(t, n, "webhook").LastTest != nil && f.tries() == 1 })
		s := statusOf(t, n, "webhook")
		if f.tries() != 1 || s.Dropped != 1 || *s.LastTest != (TestResult{ID: 9, OK: false, Error: "the chat is busy", At: s.LastTest.At}) {
			t.Fatalf("tried %d times: %+v %+v", f.tries(), s, s.LastTest)
		}
	})

	t.Run("an old event is not sent", func(t *testing.T) {
		f := &fakeSender{}
		old := ev(1, events.AccountDead)
		old.Time = time.Now().Add(-11 * time.Minute)
		n, buf := run(t, f, old)
		settled(t, "the log to say the drop", func() bool { return buf.count("events: webhook dropped 1 event: older than 10 minutes") == 1 })
		if f.tries() != 0 || statusOf(t, n, "webhook").Dropped != 1 {
			t.Fatalf("tried %d times; the log: %s", f.tries(), buf)
		}
	})
}

// An attempt is bounded twice: by attemptTimeout, after which a sink that
// honours its context gives up and is tried again; and by Wait's deadline,
// which cancels it, counts it dropped and is not the sink's failure.
func TestAnAttemptIsBounded(t *testing.T) {
	t.Run("at attemptTimeout", func(t *testing.T) {
		quick(t)
		attemptTimeout = 20 * time.Millisecond
		var mu sync.Mutex
		var bounded []bool
		var away []time.Duration // from each call to its deadline
		f := &fakeSender{answer: func(ctx context.Context, _ int, _ events.Event) error {
			dl, ok := ctx.Deadline()
			mu.Lock()
			bounded, away = append(bounded, ok), append(away, time.Until(dl))
			mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}}
		useSenders(t, map[string]any{"webhook": f})
		d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
		eventually(t, "the event to be dropped", func() bool { return statusOf(t, n, "webhook").Dropped == 1 })
		mu.Lock()
		ok, first := bounded[0], away[0]
		mu.Unlock()
		// A call that came late sees its deadline near or past; none, or a
		// later one, is the bound gone.
		if !ok || first > 20*time.Millisecond {
			t.Fatalf("the first attempt's deadline: %v, %s away; want one at most 20ms away", ok, first)
		}
		if s := statusOf(t, n, "webhook"); f.tries() != 7 || s.State != stateFailing || s.LastError != "context deadline exceeded" {
			t.Fatalf("tried %d times: %+v", f.tries(), s)
		}
	})

	t.Run("at Wait's deadline", func(t *testing.T) {
		quick(t)
		attemptTimeout = time.Hour
		// Long enough that only an attempt cancelled at the deadline comes
		// back before Wait gives up on it.
		cancelGrace = 2 * time.Second
		inflight, back := make(chan struct{}), make(chan struct{})
		f := &fakeSender{answer: func(ctx context.Context, _ int, _ events.Event) error {
			close(inflight)
			<-ctx.Done()
			close(back)
			return ctx.Err()
		}}
		useSenders(t, map[string]any{"webhook": f})
		log, buf := newLog()
		d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log})
		<-inflight
		began := time.Now()
		if n.Wait(began.Add(50 * time.Millisecond)) {
			t.Fatal("Wait said everything was sent")
		}
		select {
		case <-back:
		default:
			t.Fatal("Wait returned before the attempt it cancelled came back")
		}
		if took := time.Since(began); took > time.Second {
			t.Fatalf("Wait took %s: it did not cancel the attempt at its deadline", took)
		}
		if s := statusOf(t, n, "webhook"); s.Dropped != 1 || s.State == stateFailing || s.LastError != "" {
			t.Fatalf("%+v", s)
		}
		if buf.count("failing:") != 0 || buf.count("events: webhook dropped 1 event: the daemon was stopping") != 1 {
			t.Fatalf("the log: %s", buf)
		}
	})

	// The grace is what lets a cancelled attempt finish its own way, a
	// command being killed or an answer being read, and count itself.
	t.Run("an attempt that comes back in the grace counts itself", func(t *testing.T) {
		quick(t)
		attemptTimeout = time.Hour
		cancelGrace = 2 * time.Second
		inflight := make(chan struct{})
		f := &fakeSender{answer: func(ctx context.Context, _ int, _ events.Event) error {
			close(inflight)
			<-ctx.Done()
			time.Sleep(100 * time.Millisecond)
			return nil
		}}
		useSenders(t, map[string]any{"webhook": f})
		d := &dialer{plan: []any{newSource(ev(1, events.AccountDead))}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
		<-inflight
		n.Wait(time.Now().Add(20 * time.Millisecond))
		if s := statusOf(t, n, "webhook"); s.Sent != 1 || s.Dropped != 0 {
			t.Fatalf("sent %d, dropped %d; want it sent", s.Sent, s.Dropped)
		}
	})
}

// A test goes before the events a sink has queued, and is sent even while
// the sink waits to try an event again, so that it is answered with the
// sink's own error at once rather than after the retries.
func TestATestJumpsTheQueue(t *testing.T) {
	t.Run("while an event waits to be retried", func(t *testing.T) {
		quick(t)
		backoff = []time.Duration{time.Millisecond, time.Hour, time.Hour, time.Hour, time.Hour, time.Hour}
		f := &fakeSender{answer: func(context.Context, int, events.Event) error { return again(errors.New("dial tcp: refused"), 0) }}
		useSenders(t, map[string]any{"webhook": f})
		src := newSource(ev(1, events.AccountDead), ev(2, events.AccountDead))
		d := &dialer{plan: []any{src}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
		eventually(t, "event 1 to wait an hour", func() bool { return f.tries() == 2 && queued(n, "webhook") == 1 })

		began := time.Now()
		src.feed <- ev(9, events.Test)
		eventually(t, "the test to be answered", func() bool { return statusOf(t, n, "webhook").LastTest != nil })
		if took := time.Since(began); took > time.Second {
			t.Fatalf("the test was answered after %s", took)
		}
		s := statusOf(t, n, "webhook")
		if lt := *s.LastTest; lt.ID != 9 || lt.OK || lt.Error != "dial tcp: refused" {
			t.Fatalf("the test: %+v", lt)
		}
		if f.tries() != 3 || queued(n, "webhook") != 1 || s.Dropped != 1 {
			t.Fatalf("tried %d times, %d queued: %+v", f.tries(), queued(n, "webhook"), s)
		}
	})

	t.Run("ahead of what is queued", func(t *testing.T) {
		quick(t)
		inflight, release := make(chan struct{}), make(chan struct{})
		f := &fakeSender{answer: func(_ context.Context, call int, _ events.Event) error {
			if call == 1 {
				close(inflight)
				<-release
			}
			return nil
		}}
		useSenders(t, map[string]any{"webhook": f})
		src := newSource(ev(1, events.AccountDead))
		d := &dialer{plan: []any{src}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
		<-inflight
		src.feed <- ev(2, events.AccountDead)
		src.feed <- ev(3, events.AccountDead)
		src.feed <- ev(9, events.Test)
		eventually(t, "all three to be queued", func() bool { return queued(n, "webhook") == 3 })
		close(release)
		eventually(t, "all four to be sent", func() bool { return len(f.delivered()) == 4 })
		if got := ids(f.delivered()); !slices.Equal(got, []int64{1, 9, 2, 3}) {
			t.Fatalf("sent %v, want 1, 9, 2, 3", got)
		}
	})

	t.Run("two tests that share one poke", func(t *testing.T) {
		quick(t)
		backoff = []time.Duration{time.Hour}
		inAttempt, release := make(chan struct{}), make(chan struct{})
		f := &fakeSender{answer: func(_ context.Context, call int, e events.Event) error {
			if e.Type == events.Test {
				return nil
			}
			if call == 1 {
				close(inAttempt)
				<-release
			}
			return again(errors.New("down"), 0)
		}}
		useSenders(t, map[string]any{"webhook": f})
		src := newSource(ev(1, events.AccountDead))
		d := &dialer{plan: []any{src}}
		n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
		<-inAttempt
		src.feed <- ev(9, events.Test)
		src.feed <- ev(10, events.Test)
		eventually(t, "two tests to wait", func() bool { return queued(n, "webhook") == 2 })
		close(release)
		eventually(t, "both tests to be sent", func() bool { return len(f.delivered()) == 2 })
		if got := ids(f.delivered()); !slices.Equal(got, []int64{9, 10}) {
			t.Fatalf("sent %v, want 9, 10", got)
		}
	})
}

// A queue that is full lets its oldest event go: the event in flight goes
// on, and of five that come while it does, the last two are kept. The log
// says the first drop at once and the rest once at the end, not once each.
func TestAFullQueueDropsTheOldest(t *testing.T) {
	quick(t)
	queueDepth = 2
	inflight, release := make(chan struct{}), make(chan struct{})
	f := &fakeSender{answer: func(_ context.Context, call int, _ events.Event) error {
		if call == 1 {
			close(inflight)
			<-release
		}
		return nil
	}}
	useSenders(t, map[string]any{"webhook": f})
	log, buf := newLog()
	src := newSource(ev(1, events.SessionAttention))
	d := &dialer{plan: []any{src}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log})

	<-inflight
	for i := int64(2); i <= 6; i++ {
		src.feed <- ev(i, events.SessionAttention)
	}
	eventually(t, "three to be dropped", func() bool { return statusOf(t, n, "webhook").Dropped == 3 })
	close(release)
	eventually(t, "three to be sent", func() bool { return len(f.delivered()) == 3 })
	if got := ids(f.delivered()); !slices.Equal(got, []int64{1, 5, 6}) {
		t.Fatalf("delivered %v, want 1, 5, 6", got)
	}
	n.Wait(time.Now())
	if s := statusOf(t, n, "webhook"); s.Dropped != 3 || s.Sent != 3 {
		t.Fatalf("%+v", s)
	}
	if buf.count("events: webhook dropped 1 event: its queue was full") != 1 ||
		buf.count("events: webhook dropped 2 events: its queue was full") != 1 || buf.count("dropped") != 2 {
		t.Fatalf("the log: %s", buf)
	}
}

// A failing sink does not hold the daemon's stop: once Wait is called, an
// event waiting to be tried again is dropped at once, an event not yet
// tried gets its one attempt, and Wait returns when nothing is left.
func TestAFailingSinkDoesNotHoldTheStop(t *testing.T) {
	quick(t)
	backoff = slices.Repeat([]time.Duration{time.Hour}, 6)
	good := &fakeSender{}
	bad := &fakeSender{answer: func(context.Context, int, events.Event) error { return again(errors.New("dial tcp: refused"), 0) }}
	useSenders(t, map[string]any{"telegram": good, "webhook": bad})
	src := newSource(ev(1, events.AccountDead))
	d := &dialer{plan: []any{src}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, webhookSink}}, Dial: d.dial})
	eventually(t, "the webhook to wait an hour", func() bool { return bad.tries() == 1 && len(good.delivered()) == 1 })

	src.feed <- ev(2, events.DaemonStopped)
	src.feed <- io.EOF
	began := time.Now()
	if !n.Wait(began.Add(5 * time.Second)) {
		t.Fatal("Wait gave up")
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Wait took %s", took)
	}
	if got := ids(good.delivered()); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("telegram got %v", got)
	}
	if s := statusOf(t, n, "webhook"); bad.tries() != 2 || s.Dropped != 2 || s.State != stateFailing {
		t.Fatalf("tried %d times: %+v", bad.tries(), s)
	}
}

// Wait is bounded by its deadline whatever a sink does: one that never
// returns is given up on, and what it held counts as dropped (Review Focus 1).
func TestStopIsBoundedByAStuckSink(t *testing.T) {
	quick(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	f := &fakeSender{answer: func(context.Context, int, events.Event) error { <-stuck; return nil }}
	useSenders(t, map[string]any{"webhook": f})
	dir := t.TempDir()
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead), ev(2, events.AccountDead))}}
	n := startNotifier(t, Options{Terminals: dir, Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
	eventually(t, "one in flight and one queued", func() bool { return f.tries() == 1 && queued(n, "webhook") == 1 })

	began := time.Now()
	if n.Wait(began.Add(100 * time.Millisecond)) {
		t.Fatal("Wait said everything was sent")
	}
	if took := time.Since(began); took > 300*time.Millisecond {
		t.Fatalf("Wait took %s", took)
	}
	if s := statusOf(t, n, "webhook"); s.Dropped != 2 || s.Sent != 0 {
		t.Fatalf("%+v", s)
	}
	f2, err := readFile(StatusPath(dir))
	if err != nil || f2.Sinks[0].Dropped != 2 {
		t.Fatalf("sinks.json: %+v %v", f2, err)
	}
}

// When Wait gives up, everything a sink still holds counts as dropped: the
// event waiting to be retried, a test stuck in flight beside it, and the
// tests waiting behind that one.
func TestAGiveUpCountsEverythingLeft(t *testing.T) {
	quick(t)
	backoff = []time.Duration{time.Hour}
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	probing := make(chan struct{}, 1)
	f := &fakeSender{answer: func(_ context.Context, _ int, e events.Event) error {
		if e.Type == events.Test {
			probing <- struct{}{}
			<-stuck // ignores its context
			return nil
		}
		return again(errors.New("down"), 0)
	}}
	useSenders(t, map[string]any{"webhook": f})
	src := newSource(ev(1, events.AccountDead))
	d := &dialer{plan: []any{src}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})
	eventually(t, "event 1 to wait to be retried", func() bool { return f.tries() == 1 })
	src.feed <- ev(9, events.Test)
	<-probing
	src.feed <- ev(10, events.Test)
	src.feed <- ev(11, events.Test)
	eventually(t, "two tests to wait", func() bool { return queued(n, "webhook") == 2 })
	n.Wait(time.Now().Add(20 * time.Millisecond))
	if s := statusOf(t, n, "webhook"); s.Dropped != 4 {
		t.Fatalf("dropped %d, want 4: the event, the test in flight and two waiting", s.Dropped)
	}
}
