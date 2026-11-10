// Package notify sends the daemon's events to the sinks server.toml names
// in [[events.sink]]: a Telegram chat, a webhook, a program. It runs in the
// daemon, reads the daemon's own event stream over its control socket once,
// and gives each sink a queue of its own that retries and then drops, so a
// sink that is slow or down holds up no other sink and never the daemon.
// How each sink is doing is written to sinks.json beside the daemon's other
// files, for `rota serve status`, with no secret in it.
//
// A server.toml without a sink starts nothing here: Start returns nil.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/professor93/rota/api"
	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/events"
)

// The knobs of delivery, variables so a test can make them short. Start
// reads them once: a notifier keeps the ones it started with.
var (
	// backoff is the wait before each retry of an event; an event that has
	// failed once more than backoff has entries is dropped.
	backoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}
	// queueDepth is how many events wait for one sink; one more lets the
	// oldest of them go.
	queueDepth = 64
	// attemptTimeout bounds one attempt of any sink.
	attemptTimeout = 15 * time.Second
	// maxAge is how old an event may be and still be sent.
	maxAge = 10 * time.Minute
	// maxAfter is the longest a sink's own "try again after" is waited for.
	maxAfter = 5 * time.Minute
	// statusEvery is how often, at most, sinks.json is written.
	statusEvery = time.Second
	// redialMax is the longest wait before the stream is opened again.
	redialMax = 30 * time.Second
	// dropSayEvery is how often, at most, the log hears of one sink's drops.
	dropSayEvery = time.Minute
	// cancelGrace is how long Wait, having cancelled the attempts under way
	// at its deadline, gives them to come back and count themselves before
	// it counts them itself.
	cancelGrace = 50 * time.Millisecond
	// unreachedSay is how many failed dials in a row the log hears of, once.
	unreachedSay = 3
)

// knobs is the knobs as one notifier read them.
type knobs struct {
	backoff                                  []time.Duration
	depth                                    int
	attempt, maxAge, maxAfter, every, redial time.Duration
	dropSay, grace                           time.Duration
	unreached                                int
}

func current() knobs {
	return knobs{
		backoff: slices.Clone(backoff), depth: max(queueDepth, 1),
		attempt: attemptTimeout, maxAge: maxAge, maxAfter: maxAfter,
		every: max(statusEvery, time.Millisecond), redial: redialMax, dropSay: dropSayEvery,
		grace: cancelGrace, unreached: max(unreachedSay, 1),
	}
}

// Options is what Start needs. Events is server.toml's [events]; Terminals
// the store's terminals directory, where the control socket and sinks.json
// are; Log the daemon's log; Version rota's, for a sink that says who sends;
// HideEnv the variables a command sink's program must not see. Dial opens
// the daemon's event stream from since; nil is the control socket.
type Options struct {
	Events    api.EventsSection
	Terminals string
	Log       *slog.Logger
	Version   string
	HideEnv   []string
	Dial      func(ctx context.Context, since int64) (Source, error)
}

// Source is an event stream as *client.EventStream is one. Last is the
// highest id it has read, to open it again from; a gap never moves it.
// Close makes a Next that waits return. Close may be called more than once,
// and from another goroutine while Next waits.
type Source interface {
	Next() (events.Event, error)
	Last() int64
	Close() error
}

var _ Source = (*client.EventStream)(nil)

// sender is one sink's way of sending one event.
//
// send is bounded by ctx, which ends at attemptTimeout or when Wait gives
// up, and must return soon after ctx ends: a send that does not is
// abandoned by Wait, its goroutine leaks, and nothing it does afterwards
// counts. A failure it can classify is again or dropped; any other is tried
// again. Its error never quotes a response body beyond its status line,
// unless every form the body could echo a secret in is among secrets.
//
// secrets is what must never show in anything said of the sink, asked once,
// when the sink is made; every error send returns goes through cleanErr
// with them.
type sender interface {
	send(ctx context.Context, e events.Event) error
	secrets() []string
}

// env is what a sink is made with besides its own keys: rota's version,
// [events] link, the variables a program must not see, and the log.
type env struct {
	version, link string
	hide          []string
	log           *slog.Logger
}

// newSender makes the sink s is, through its type's constructor. Each
// constructor keeps this contract: its error may name a file but never
// quotes anything read from one. A constructor that fails after reading a
// secret may return its sender with the error, so that its secrets() clean
// the error; such a sender is never sent anything. Every build error is
// cleaned with the secrets of every sink that was made, and with the URL of
// its own sink (configSecrets).
func newSender(s api.EventSink, e env) (sender, error) {
	switch s.Type {
	case "webhook":
		return newWebhook(s, e)
	case "telegram":
		return newTelegram(s, e)
	case "command":
		return newCommand(s, e)
	}
	return nil, fmt.Errorf("type %q is not a sink", s.Type)
}

// makeSender is newSender, a variable so a test can make its own sinks.
var makeSender = newSender

// retry is a failed attempt to try again, after at least after.
type retry struct {
	err   error
	after time.Duration
}

func (r *retry) Error() string { return r.err.Error() }
func (r *retry) Unwrap() error { return r.err }

// drop is a failed attempt that trying again would not mend.
type drop struct{ err error }

func (d *drop) Error() string { return d.err.Error() }
func (d *drop) Unwrap() error { return d.err }

// again says the attempt that failed with err may be tried again, and not
// before after (0 is backoff's own wait). A failure that is neither again
// nor dropped is tried again too.
func again(err error, after time.Duration) error {
	if err == nil {
		err = errors.New("the attempt failed")
	}
	return &retry{err, after}
}

// dropped says the attempt that failed with err is not to be tried again.
func dropped(err error) error {
	if err == nil {
		err = errors.New("the attempt failed")
	}
	return &drop{err}
}

// sinkNames is what each sink is called: its type, and for the second and
// later of one type, the type and its place among them (telegram,
// telegram-2). Adding a sink never renames one written above it. Every
// name this package says or matches comes from here.
func sinkNames(sinks []api.EventSink) []string {
	seen := map[string]int{}
	out := make([]string, len(sinks))
	for i, s := range sinks {
		seen[s.Type]++
		out[i] = s.Type
		if n := seen[s.Type]; n > 1 {
			out[i] = fmt.Sprintf("%s-%d", s.Type, n)
		}
	}
	return out
}

// Notifier is the sinks of one daemon, at work. It runs until the daemon's
// stream ends after daemon.stopped, or until Wait gives up on it.
type Notifier struct {
	k     knobs
	log   *slog.Logger
	dial  func(ctx context.Context, since int64) (Source, error)
	path  string
	sinks []*sink

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{} // closed when Wait is called: nothing is tried again
	read   chan struct{} // closed when the reader is done
	done   chan struct{} // closed when the reader and every queue are

	dirty      atomic.Bool
	writerStop chan struct{}
	writerDone chan struct{}
	writeMu    sync.Mutex
	writeFail  bool

	waitOnce sync.Once
	waitOK   bool
}

// Start makes each sink and starts reading the daemon's events for them. No
// sink is nil, nil: nothing runs, nothing is written, nothing is logged. A
// sink that cannot be made is failing from the start, says why in the log
// and in sinks.json, and is sent nothing; it never stops the others.
func Start(o Options) (*Notifier, error) {
	if len(o.Events.Sinks) == 0 {
		return nil, nil
	}
	if o.Terminals == "" {
		return nil, errors.New("events: no terminals directory to keep sinks.json in")
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dial := o.Dial
	if dial == nil {
		dial = socket(o.Terminals)
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &Notifier{
		k: current(), log: log, dial: dial, path: StatusPath(o.Terminals),
		ctx: ctx, cancel: cancel, stop: make(chan struct{}), read: make(chan struct{}), done: make(chan struct{}),
		writerStop: make(chan struct{}), writerDone: make(chan struct{}),
	}
	e := env{version: o.Version, link: o.Events.Link, hide: slices.Clone(o.HideEnv), log: log}
	names := sinkNames(o.Events.Sinks)
	// Every sink is made before any error is said, so that each is cleaned
	// with the secrets of all that were made.
	errs := make([]error, len(o.Events.Sinks))
	var made []string
	for i, cfg := range o.Events.Sinks {
		s := &sink{n: n, wake: make(chan struct{}, 1), poke: make(chan struct{}, 1), on: slices.Clone(cfg.On), st: Status{
			Name: names[i], Type: cfg.Type, Target: targetOf(cfg), On: patterns(cfg.On), State: stateWaiting,
		}}
		s.keys = configSecrets(cfg)
		snd, err := makeSender(cfg, e)
		if snd != nil {
			s.keys = append(slices.Clone(snd.secrets()), s.keys...)
		}
		if err == nil && snd == nil {
			err = errors.New("the sink could not be made")
		}
		if errs[i] = err; err == nil {
			s.snd = snd
			made = append(made, s.keys...)
		}
		n.sinks = append(n.sinks, s)
	}
	now := time.Now().UTC()
	for i, s := range n.sinks {
		if errs[i] != nil {
			s.st.State, s.st.LastError, s.st.LastErrorAt = stateFailing, cleanErr(errs[i], slices.Concat(s.keys, made)), now
			log.Warn(fmt.Sprintf("events: %s failing: %s", s.st.Name, s.st.LastError))
		}
	}
	n.writeStatus()
	var workers sync.WaitGroup
	for _, s := range n.sinks {
		if s.snd != nil {
			workers.Go(s.work)
		}
	}
	go n.reader()
	go func() {
		<-n.read
		workers.Wait()
		close(n.done)
	}()
	go n.keepStatus()
	return n, nil
}

// socket is Options.Dial for the daemon's control socket.
func socket(terminals string) func(ctx context.Context, since int64) (Source, error) {
	return func(ctx context.Context, since int64) (Source, error) {
		c, err := client.DialNow(terminals)
		if err != nil {
			return nil, err
		}
		s, err := c.Events(ctx, since)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
}

// Wait is the daemon stopping: it waits for the reader to finish and every
// queue to empty, and says whether they did by until.
//
// From the moment it is called nothing is tried again, so a sink that is
// failing does not hold the stop: an event waiting to be retried is dropped
// at once, and an event not yet tried gets its one attempt. The stream is
// read to its end, and not opened again once it ends.
//
// At until it gives up. Attempts under way are cancelled and given
// cancelGrace to come back; then what is left, queued or in flight, counts
// as dropped, and sinks.json is written once more. An event the reader
// still routes after that is counted in memory but not written down.
//
// A nil notifier has nothing to wait for. Wait is for one caller, once; a
// later call says what the first did.
func (n *Notifier) Wait(until time.Time) bool {
	if n == nil {
		return true
	}
	n.waitOnce.Do(func() {
		close(n.stop)
		t := time.NewTimer(time.Until(until))
		defer t.Stop()
		select {
		case <-n.done:
			n.waitOK = true
		default:
			select {
			case <-n.done:
				n.waitOK = true
			case <-t.C:
				n.cancel()
				g := time.NewTimer(n.k.grace)
				select {
				case <-n.done:
				case <-g.C:
				}
				g.Stop()
				now := time.Now().UTC()
				for _, s := range n.sinks {
					s.finish(now)
				}
			}
		}
		n.cancel()
		close(n.writerStop)
		<-n.writerDone
		n.sayDrops(true)
		n.writeStatus()
	})
	return n.waitOK
}

// reader reads the daemon's stream for as long as there is one: from its
// start first, then from the last event read, after a wait that grows to
// redialMax. Once daemon.stopped has been routed the stream is read to its
// end, since an event can land after it, and not opened again; nor is it
// once Wait has been called.
//
// A refusal (401 or 403) is said once each time the daemon starts to
// refuse. Any other failure to open the stream is said once it has failed
// unreachedSay times in a row, and the stream opening again after that is
// said too.
func (n *Notifier) reader() {
	defer close(n.read)
	since, fails, refused := int64(0), 0, false
	unreached, lost := 0, false
	for n.ctx.Err() == nil {
		src, err := n.dial(n.ctx, since)
		if err != nil {
			var r *client.Refusal
			no := errors.As(err, &r) && (r.Status == http.StatusUnauthorized || r.Status == http.StatusForbidden)
			if no && !refused {
				n.log.Warn("events: the daemon refused the stream: " + cleanErr(err, nil))
			}
			refused = no
			unreached++
			if no {
				unreached = 0 // reached, and refused
			}
			if unreached == n.k.unreached && !lost {
				lost = true
				n.log.Warn("events: cannot reach the daemon's event stream: " + cleanErr(err, nil))
			}
			if !n.pause(fails) {
				return
			}
			fails++
			continue
		}
		refused, unreached = false, 0
		if lost {
			lost = false
			n.log.Info("events: the daemon's event stream is back")
		}
		read, stopped := n.drain(src)
		since = max(since, src.Last())
		src.Close()
		if stopped {
			return
		}
		if read > 0 {
			fails = 0
		}
		if !n.pause(fails) {
			return
		}
		fails++
	}
}

// drain routes each event of src until it ends, and says how many it read
// and whether daemon.stopped was among them.
func (n *Notifier) drain(src Source) (read int, stopped bool) {
	stop := context.AfterFunc(n.ctx, func() { src.Close() })
	defer stop()
	for {
		e, err := src.Next()
		if err != nil {
			return read, stopped
		}
		read++
		if e.Type == events.Gap {
			n.log.Warn("events: some events were missed")
			continue
		}
		n.route(e)
		if e.Type == events.DaemonStopped {
			stopped = true
		}
	}
}

// pause waits before the stream is opened again for the time after fails
// failures, and says false when the daemon is stopping instead.
func (n *Notifier) pause(fails int) bool {
	d := n.k.redial
	if len(n.k.backoff) > 0 {
		d = min(n.k.backoff[min(fails, len(n.k.backoff)-1)], n.k.redial)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-n.stop:
		return false
	case <-n.ctx.Done():
		return false
	}
}

// route gives e to each sink it is for: a test to the sink it names, or to
// every sink, and anything else to each sink whose on matches.
func (n *Notifier) route(e events.Event) {
	for _, s := range n.sinks {
		if e.Type == events.Test {
			if e.Sink != "" && e.Sink != s.st.Name {
				continue
			}
		} else if !events.Match(s.on, e.Type) {
			continue
		}
		n.say(s.put(e))
	}
}

// line is one thing to say in the log, once no lock is held.
type line struct {
	warn bool
	msg  string
}

func (n *Notifier) say(lines []line) {
	for _, l := range lines {
		if l.warn {
			n.log.Warn(l.msg)
		} else {
			n.log.Info(l.msg)
		}
	}
}

// sayDrops says each sink's drops not said yet, when a minute has passed
// since its last line about them, or now when all is true.
func (n *Notifier) sayDrops(all bool) {
	now := time.Now().UTC()
	for _, s := range n.sinks {
		s.mu.Lock()
		lines := s.sayDropsLocked(now, all)
		s.mu.Unlock()
		n.say(lines)
	}
}

// status is how each sink stands now.
func (n *Notifier) status() []Status {
	out := make([]Status, 0, len(n.sinks))
	for _, s := range n.sinks {
		s.mu.Lock()
		st := s.st
		st.On = slices.Clone(st.On)
		if st.LastTest != nil {
			t := *st.LastTest
			st.LastTest = &t
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	return out
}
