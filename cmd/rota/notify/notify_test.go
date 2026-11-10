package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/events"
)

// The helpers below are this package's own. Tasks that add a sink keep
// theirs apart by a prefix (hook…, tg…, cmd…).

// quick makes every wait of delivery a millisecond, for one test, and puts
// the knobs back after it.
func quick(t *testing.T) {
	t.Helper()
	b, q, a, age, after, every, redial, say := backoff, queueDepth, attemptTimeout, maxAge, maxAfter, statusEvery, redialMax, dropSayEvery
	grace, unreached := cancelGrace, unreachedSay
	t.Cleanup(func() {
		backoff, queueDepth, attemptTimeout, maxAge, maxAfter, statusEvery, redialMax, dropSayEvery = b, q, a, age, after, every, redial, say
		cancelGrace, unreachedSay = grace, unreached
	})
	backoff = slices.Repeat([]time.Duration{time.Millisecond}, 6)
	redialMax = time.Millisecond
	statusEvery = 5 * time.Millisecond
}

// fakeSource is a stream a test feeds: each item is an events.Event to give,
// or an error to end with. Last is the highest id given, as a client's
// stream says it, and a gap never moves it.
type fakeSource struct {
	feed   chan any
	last   atomic.Int64
	closed chan struct{}
	once   sync.Once
}

func newSource(items ...any) *fakeSource {
	s := &fakeSource{feed: make(chan any, 256), closed: make(chan struct{})}
	for _, it := range items {
		s.feed <- it
	}
	return s
}

func (s *fakeSource) Next() (events.Event, error) {
	select {
	case <-s.closed:
		return events.Event{}, client.ErrStreamClosed
	case it := <-s.feed:
		if err, ok := it.(error); ok {
			return events.Event{}, err
		}
		e := it.(events.Event)
		if e.Type != events.Gap && e.ID > s.last.Load() {
			s.last.Store(e.ID)
		}
		return e, nil
	}
}

func (s *fakeSource) Last() int64 { return s.last.Load() }

func (s *fakeSource) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// dialer is Options.Dial for a test: each call takes the next of plan, a
// *fakeSource or an error, and past its end a stream that never ends. It
// keeps the since and the time of every call.
type dialer struct {
	mu     sync.Mutex
	sinces []int64
	at     []time.Time
	plan   []any
}

func (d *dialer) dial(_ context.Context, since int64) (Source, error) {
	d.mu.Lock()
	i := len(d.sinces)
	d.sinces = append(d.sinces, since)
	d.at = append(d.at, time.Now())
	d.mu.Unlock()
	if i < len(d.plan) {
		switch v := d.plan[i].(type) {
		case error:
			return nil, v
		case *fakeSource:
			return v, nil
		}
	}
	return newSource(), nil
}

func (d *dialer) asked() []int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.sinces)
}

func (d *dialer) when() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.at)
}

// fakeSender is a sink that keeps what reached it. answer says how each
// call goes, by its number from 1; nil delivers every one.
type fakeSender struct {
	answer func(ctx context.Context, call int, e events.Event) error
	keys   []string

	mu    sync.Mutex
	calls int
	at    []time.Time
	got   []events.Event
}

func (f *fakeSender) send(ctx context.Context, e events.Event) error {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.at = append(f.at, time.Now())
	f.mu.Unlock()
	var err error
	if f.answer != nil {
		err = f.answer(ctx, call, e)
	}
	if err == nil {
		f.mu.Lock()
		f.got = append(f.got, e)
		f.mu.Unlock()
	}
	return err
}

func (f *fakeSender) secrets() []string { return f.keys }

func (f *fakeSender) delivered() []events.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func (f *fakeSender) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeSender) times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.at)
}

// useSenders makes the sinks of each type out of byType: a sender, an error
// to fail with, a func(api.EventSink) error that fails with what it says,
// or a whole constructor.
func useSenders(t *testing.T, byType map[string]any) {
	t.Helper()
	was := makeSender
	t.Cleanup(func() { makeSender = was })
	makeSender = func(s api.EventSink, e env) (sender, error) {
		switch v := byType[s.Type].(type) {
		case sender:
			return v, nil
		case error:
			return nil, v
		case func(api.EventSink) error:
			return nil, v(s)
		case func(api.EventSink, env) (sender, error):
			return v(s, e)
		}
		return nil, fmt.Errorf("this test has no sender for a %s sink", s.Type)
	}
}

// logBuffer is a log the notifier's goroutines may all write to.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuffer) count(s string) int { return strings.Count(l.String(), s) }

func newLog() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewTextHandler(b, nil)), b
}

// startNotifier starts one, in a terminals directory of its own unless o
// names one, and stops it when the test ends.
func startNotifier(t *testing.T, o Options) *Notifier {
	t.Helper()
	if o.Terminals == "" {
		o.Terminals = t.TempDir()
	}
	n, err := Start(o)
	if err != nil || n == nil {
		t.Fatalf("Start: %v %v", n, err)
	}
	t.Cleanup(func() { n.Wait(time.Now()) })
	return n
}

// eventually waits up to five seconds for cond.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited 5s for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// settled checks that cond, once true, is still true a moment later: that
// nothing more came.
func settled(t *testing.T, what string, cond func() bool) {
	t.Helper()
	eventually(t, what, cond)
	time.Sleep(20 * time.Millisecond)
	if !cond() {
		t.Fatalf("%s, and then no longer", what)
	}
}

// statusOf is what the notifier says of the sink called name.
func statusOf(t *testing.T, n *Notifier, name string) Status {
	t.Helper()
	for _, s := range n.status() {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no sink is called %q", name)
	return Status{}
}

// queued is how many events and tests wait for the sink called name.
func queued(n *Notifier, name string) int {
	for _, s := range n.sinks {
		if s.st.Name == name {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.queue) + len(s.tests)
		}
	}
	return -1
}

func ev(id int64, typ string) events.Event {
	return events.Event{ID: id, Type: typ, Time: time.Now().UTC(), Name: "work@x.com", Text: fmt.Sprintf("said %d", id)}
}

func ids(evs []events.Event) []int64 {
	out := []int64{}
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

var (
	telegramSink = api.EventSink{Type: "telegram", BotTokenFile: "/nowhere/tg.token", ChatID: "123"}
	webhookSink  = api.EventSink{Type: "webhook", URL: "https://hooks.example/h"}
)

// No sink is no notifier: nothing runs, no file is written, and the log
// hears nothing about events. A nil notifier has nothing to wait for.
func TestNoSinkStartsNothing(t *testing.T) {
	dir := t.TempDir()
	log, buf := newLog()
	for _, o := range []Options{{}, {Terminals: dir, Log: log}, {Terminals: dir, Log: log, Events: api.EventsSection{Link: "https://box:8787"}}} {
		n, err := Start(o)
		if n != nil || err != nil {
			t.Fatalf("Start(%+v) = %v, %v; want nil, nil", o, n, err)
		}
		if !n.Wait(time.Now().Add(time.Hour)) {
			t.Fatal("a nil notifier's Wait said false")
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the terminals directory has %v", entries)
	}
	if buf.String() != "" {
		t.Fatalf("the log heard %q", buf.String())
	}
}

// Each sink is made with what the daemon says of itself: rota's version,
// [events] link, the variables a program must not see, and its log.
func TestSendersAreMadeWithTheDaemonsWords(t *testing.T) {
	quick(t)
	var got []env
	var mu sync.Mutex
	useSenders(t, map[string]any{"webhook": func(_ api.EventSink, e env) (sender, error) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		return &fakeSender{}, nil
	}})
	log, _ := newLog()
	startNotifier(t, Options{
		Events:  api.EventsSection{Link: "https://box.example:8787", Sinks: []api.EventSink{webhookSink, webhookSink}},
		Version: "1.4.0-test", HideEnv: []string{"ROTA_TOKEN", "MY_KEY"}, Log: log, Dial: (&dialer{}).dial,
	})
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("made %d senders", len(got))
	}
	for _, e := range got {
		if e.version != "1.4.0-test" || e.link != "https://box.example:8787" || !slices.Equal(e.hide, []string{"ROTA_TOKEN", "MY_KEY"}) || e.log != log {
			t.Fatalf("a sender was made with %+v", e)
		}
	}
}

// A sink is called by its type, and the second and later of one type by
// their place among that type; adding one never renames one above it.
func TestSinkNamesAreTheirTypes(t *testing.T) {
	cmd := api.EventSink{Type: "command", Command: []string{"notify-send"}}
	got := sinkNames([]api.EventSink{telegramSink, webhookSink, telegramSink, cmd, telegramSink})
	want := []string{"telegram", "webhook", "telegram-2", "command", "telegram-3"}
	if !slices.Equal(got, want) {
		t.Fatalf("sinkNames = %q, want %q", got, want)
	}
	if got := sinkNames([]api.EventSink{telegramSink, webhookSink, telegramSink}); !slices.Equal(got, want[:3]) {
		t.Fatalf("sinkNames = %q", got)
	}
}

// Each event goes to the sinks whose on asks for it, and an empty on asks
// for every one. What the event says, its name the first, is passed on as
// it came.
func TestEventsReachTheSinksTheyAreFor(t *testing.T) {
	quick(t)
	a, b := &fakeSender{}, &fakeSender{}
	useSenders(t, map[string]any{"telegram": a, "webhook": b})
	tg := telegramSink
	tg.On = []string{"account.*"}
	named := ev(2, events.SessionEnded)
	named.Session, named.Name = "00000000000000a1", "api work"
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead), named, ev(3, events.ViewerJoined))}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{tg, webhookSink}}, Dial: d.dial})

	eventually(t, "the webhook to have all three", func() bool { return len(b.delivered()) == 3 && len(a.delivered()) == 1 })
	if got := ids(b.delivered()); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Fatalf("the webhook got %v", got)
	}
	if got := a.delivered(); got[0].Type != events.AccountDead {
		t.Fatalf("the telegram sink got %+v", got)
	}
	if got := b.delivered()[1]; got.Name != "api work" || got.Session != "00000000000000a1" || got.Text != "said 2" {
		t.Fatalf("the event was changed on its way: %+v", got)
	}
}

// A test event goes to the sink it names whatever that sink's on says, and
// one that names none goes to every sink.
func TestATestEventGoesToTheSinkItNames(t *testing.T) {
	quick(t)
	a, b := &fakeSender{}, &fakeSender{}
	useSenders(t, map[string]any{"telegram": a, "webhook": b})
	tg, hook := telegramSink, webhookSink
	tg.On, hook.On = []string{"account.*"}, []string{"session.ended"}
	forB := ev(1, events.Test)
	forB.Sink = "webhook"
	nobody := ev(2, events.Test)
	nobody.Sink = "webhook-2"
	forAll := ev(3, events.Test)
	d := &dialer{plan: []any{newSource(forB, nobody, forAll)}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{tg, hook}}, Dial: d.dial})

	eventually(t, "both tests to land", func() bool { return len(b.delivered()) == 2 && len(a.delivered()) == 1 })
	if got := ids(b.delivered()); !slices.Equal(got, []int64{1, 3}) {
		t.Fatalf("the webhook got %v", got)
	}
	if got := ids(a.delivered()); !slices.Equal(got, []int64{3}) {
		t.Fatalf("the telegram sink got %v", got)
	}
}

// A stream that breaks is opened again from the last event it gave: from
// the start first, then from 5, and a gap, said once in the log, does not
// take it back to 0.
func TestTheStreamComesBackFromWhereItWas(t *testing.T) {
	quick(t)
	f := &fakeSender{}
	useSenders(t, map[string]any{"webhook": f})
	log, buf := newLog()
	broke := errors.New("the stream broke")
	first := newSource(ev(1, events.AccountDead), ev(2, events.AccountDead), ev(3, events.AccountDead),
		ev(4, events.AccountDead), ev(5, events.AccountDead), broke)
	second := newSource(events.Event{Type: events.Gap}, ev(6, events.AccountDead), broke)
	d := &dialer{plan: []any{first, second}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log})

	eventually(t, "a third stream", func() bool { return len(d.asked()) >= 3 && len(f.delivered()) == 6 })
	if got := d.asked()[:3]; !slices.Equal(got, []int64{0, 5, 6}) {
		t.Fatalf("the streams were opened from %v, want 0, 5, 6", got)
	}
	if got := ids(f.delivered()); !slices.Equal(got, []int64{1, 2, 3, 4, 5, 6}) {
		t.Fatalf("delivered %v", got)
	}
	if buf.count("events: some events were missed") != 1 {
		t.Fatalf("the log: %s", buf)
	}
}

// A daemon that refuses the stream is said once each time it starts to,
// not on every try, and an answer that is not a refusal is not said.
func TestARefusedStreamIsSaidOnce(t *testing.T) {
	quick(t)
	backoff = slices.Repeat([]time.Duration{5 * time.Millisecond}, 6)
	redialMax = time.Second
	f := &fakeSender{}
	useSenders(t, map[string]any{"webhook": f})
	log, buf := newLog()
	no := func(status int) error {
		return &client.Refusal{Status: status, Message: "this sign-in may not read events"}
	}
	d := &dialer{plan: []any{no(401), no(401), newSource(ev(1, events.AccountDead), io.EOF), no(403), no(401),
		errors.New("the rota daemon is not answering"), no(403)}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log})

	eventually(t, "the stream to be open at last", func() bool { return len(d.asked()) >= 8 && len(f.delivered()) == 1 })
	if got := buf.count("events: the daemon refused the stream: this sign-in may not read events"); got != 3 {
		t.Fatalf("said %d times; the log: %s", got, buf)
	}
	if strings.Contains(buf.String(), "not answering") {
		t.Fatalf("the log: %s", buf)
	}
	// Not in a tight loop: each try waits its backoff after the last.
	at := d.when()
	for i := 1; i < 8; i++ {
		if gap := at[i].Sub(at[i-1]); gap < 5*time.Millisecond {
			t.Fatalf("dial %d came %s after the one before", i+1, gap)
		}
	}
}

// A stream that cannot be opened is said once it has failed three times in
// a row, and its coming back is said once; fewer failures say nothing.
func TestAnUnreachableStreamIsSaidOnce(t *testing.T) {
	quick(t)
	f := &fakeSender{}
	useSenders(t, map[string]any{"webhook": f})
	log, buf := newLog()
	down := errors.New("the rota daemon is not answering")
	d := &dialer{plan: []any{down, down, newSource(ev(1, events.AccountDead), io.EOF),
		down, down, down, down, down, newSource(ev(2, events.AccountDead))}}
	startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial, Log: log})

	settled(t, "the log to say it is back", func() bool { return buf.count("events: the daemon's event stream is back") == 1 })
	if got := buf.count("events: cannot reach the daemon's event stream: the rota daemon is not answering"); got != 1 || len(f.delivered()) != 2 {
		t.Fatalf("said %d times, %d delivered; the log: %s", got, len(f.delivered()), buf)
	}
}

// A sink that cannot be made is failing from the start, says why once, and
// is sent nothing; a test for it is answered with why at once.
func TestASinkThatCannotBeBuiltIsFailing(t *testing.T) {
	quick(t)
	f := &fakeSender{}
	useSenders(t, map[string]any{"telegram": f, "webhook": errors.New("the secret file is open to others")})
	log, buf := newLog()
	d := &dialer{plan: []any{newSource(ev(1, events.AccountDead), ev(2, events.Test))}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{telegramSink, webhookSink}}, Dial: d.dial, Log: log})

	eventually(t, "both to reach telegram", func() bool { return len(f.delivered()) == 2 })
	eventually(t, "the test to be answered", func() bool { return statusOf(t, n, "webhook").LastTest != nil })
	s := statusOf(t, n, "webhook")
	if s.State != stateFailing || s.LastError != "the secret file is open to others" || s.Sent != 0 || s.Dropped != 0 {
		t.Fatalf("%+v", s)
	}
	if lt := *s.LastTest; lt.ID != 2 || lt.OK || lt.Error != s.LastError {
		t.Fatalf("its test: %+v", lt)
	}
	if buf.count("events: webhook failing: the secret file is open to others") != 1 {
		t.Fatalf("the log: %s", buf)
	}
}

// daemon.stopped is the end: the stream is read on to its end, since an
// event can land after it, and is not opened again (Review Focus 5).
func TestDaemonStoppedEndsTheRun(t *testing.T) {
	quick(t)
	f := &fakeSender{}
	useSenders(t, map[string]any{"webhook": f})
	d := &dialer{plan: []any{newSource(ev(1, events.DaemonStopped), ev(2, events.SessionEnded), io.EOF)}}
	n := startNotifier(t, Options{Events: api.EventsSection{Sinks: []api.EventSink{webhookSink}}, Dial: d.dial})

	if !n.Wait(time.Now().Add(5 * time.Second)) {
		t.Fatal("Wait gave up")
	}
	if got := ids(f.delivered()); !slices.Equal(got, []int64{1, 2}) {
		t.Fatalf("delivered %v", got)
	}
	if got := d.asked(); len(got) != 1 {
		t.Fatalf("the stream was opened %d times", len(got))
	}
}
