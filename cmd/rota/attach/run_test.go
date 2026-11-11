package attach

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/vtscan"
)

// The helpers for every test of the attach loop, in run_*_test.go by topic.

// runKnobs shortens every timing an attach waits on, for one test.
func runKnobs(t *testing.T) {
	t.Helper()
	runSet(t, &drainFor, 20*time.Millisecond)
	runSet(t, &noteFor, 150*time.Millisecond)
	runSet(t, &teachFor, 300*time.Millisecond)
	runSet(t, &redialFor, 2*time.Second)
	runSet(t, &redialEvery, 10*time.Millisecond)
	runSet(t, &forceAfter, 200*time.Millisecond)
	runSet(t, &settleFor, 100*time.Millisecond)
	runSet(t, &hintAfter, 40*time.Millisecond)
	runSet(t, &cropEvery, 5*time.Millisecond)
	runSet(t, &linkCloseWait, time.Second)
}

// runSet sets a knob for one test.
func runSet(t *testing.T, p *time.Duration, d time.Duration) {
	old := *p
	*p = d
	t.Cleanup(func() { *p = old })
}

// runTerm is the person's terminal: what the test types is sent on Input,
// and what the attach writes is kept.
type runTerm struct {
	in chan []byte

	mu         sync.Mutex
	out        bytes.Buffer
	cols, rows int
	fail       error // the next write fails with it, once
	resized    chan struct{}
}

func runNewTerm(cols, rows int) *runTerm {
	return &runTerm{in: make(chan []byte), cols: cols, rows: rows, resized: make(chan struct{}, 1)}
}

func (f *runTerm) Input() <-chan []byte { return f.in }

func (f *runTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail; err != nil {
		f.fail = nil
		return 0, err
	}
	return f.out.Write(p)
}

func (f *runTerm) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cols, f.rows
}

func (f *runTerm) Resized() <-chan struct{} { return f.resized }

// typ types s, and returns once the attach has received it.
func (f *runTerm) typ(t *testing.T, s string) {
	t.Helper()
	select {
	case f.in <- []byte(s):
	case <-time.After(5 * time.Second):
		t.Fatalf("typing %q: nobody received it", s)
	}
}

func (f *runTerm) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

func (f *runTerm) resize(cols, rows int) {
	f.mu.Lock()
	f.cols, f.rows = cols, rows
	f.mu.Unlock()
	f.resized <- struct{}{}
}

// runAfter is what the attach wrote after the last time it wrote mark.
func (f *runTerm) runAfter(mark string) string {
	out := f.output()
	i := strings.LastIndex(out, mark)
	if i < 0 {
		return ""
	}
	return out[i+len(mark):]
}

// runNext is one thing a scripted link's Next gives.
type runNext struct {
	f   client.Frame
	err error
}

// runLink is a scripted attach to one session.
type runLink struct {
	hello  client.Hello
	frames chan runNext
	done   chan struct{}
	once   sync.Once
	block  chan struct{} // when set, Type waits for it to close

	mu        sync.Mutex
	calls     []string // every Type, Claim, Fit and Close, in order
	typed     []byte
	fits      [][2]uint16
	claims    []bool
	closed    bool
	typedLate bool // typed into after Close
}

func runNewLink(id string, cols, rows uint16) *runLink {
	return &runLink{
		hello:  client.Hello{ID: id, Kind: "held", Cols: cols, Rows: rows, You: client.Viewer{Name: "me", Role: "control", Conn: "c1"}},
		frames: make(chan runNext, 64),
		done:   make(chan struct{}),
	}
}

func (l *runLink) Hello() client.Hello { return l.hello }

func (l *runLink) Next() (client.Frame, error) {
	select {
	case n := <-l.frames:
		return n.f, n.err
	case <-l.done:
		return client.Frame{}, net.ErrClosed
	}
}

func (l *runLink) Type(p []byte) error {
	if l.block != nil {
		<-l.block
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.typedLate = true
		return net.ErrClosed
	}
	l.calls = append(l.calls, "Type:"+string(p))
	l.typed = append(l.typed, p...)
	return nil
}

func (l *runLink) Fit(cols, rows uint16) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprintf("Fit:%dx%d", cols, rows))
	l.fits = append(l.fits, [2]uint16{cols, rows})
	return nil
}

func (l *runLink) Claim(force bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, fmt.Sprint("Claim:", force))
	l.claims = append(l.claims, force)
	return nil
}

func (l *runLink) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
		l.calls = append(l.calls, "Close")
		l.mu.Unlock()
		close(l.done)
	})
	return nil
}

func (l *runLink) out(s string) { l.frames <- runNext{f: client.Frame{Output: []byte(s)}} }

func (l *runLink) doc(d client.Doc) { l.frames <- runNext{f: client.Frame{Doc: &d}} }

func (l *runLink) fail(err error) { l.frames <- runNext{err: err} }

func (l *runLink) gotTyped() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.typed)
}

func (l *runLink) gotFits() [][2]uint16 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([][2]uint16(nil), l.fits...)
}

func (l *runLink) gotClaims() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.claims...)
}

func (l *runLink) gotCalls() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *runLink) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *runLink) late() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.typedLate
}

// runSrc is a scripted daemon: each open of an id takes the next of its
// links, or fails as told.
type runSrc struct {
	mu       sync.Mutex
	sessions []client.Session
	links    map[string][]*runLink
	refuse   map[string]error
	gate     map[string]chan struct{} // an open of the id waits for this
	listGate chan struct{}            // when set, a list waits for it
	lists    int                      // lists begun
	begun    []string                 // every open, as it began
	opens    []string                 // every open, as it was answered
	watched  []bool
}

func runNewSrc(sessions ...client.Session) *runSrc {
	return &runSrc{sessions: sessions, links: map[string][]*runLink{}, refuse: map[string]error{}, gate: map[string]chan struct{}{}}
}

func (s *runSrc) add(l *runLink) *runLink {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.links[l.hello.ID] = append(s.links[l.hello.ID], l)
	return l
}

func (s *runSrc) open(id string, watch bool) (link, error) {
	s.mu.Lock()
	s.begun = append(s.begun, id)
	g := s.gate[id]
	s.mu.Unlock()
	if g != nil {
		<-g
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens = append(s.opens, id)
	s.watched = append(s.watched, watch)
	if err := s.refuse[id]; err != nil {
		return nil, err
	}
	ls := s.links[id]
	if len(ls) == 0 {
		return nil, &client.Refusal{Status: 404, Message: "no such session"}
	}
	s.links[id] = ls[1:]
	return ls[0], nil
}

func (s *runSrc) list() ([]client.Session, error) {
	s.mu.Lock()
	s.lists++
	g := s.listGate
	s.mu.Unlock()
	if g != nil {
		<-g
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]client.Session(nil), s.sessions...), nil
}

func (s *runSrc) setRefuse(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[id] = err
}

func (s *runSrc) setGate(id string) chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := make(chan struct{})
	s.gate[id] = g
	return g
}

func (s *runSrc) gotOpens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opens...)
}

func (s *runSrc) gotBegun() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.begun...)
}

func (s *runSrc) gotWatched() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.watched...)
}

var runT0 = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// runSession is a live, attachable session started n minutes after runT0.
func runSession(id string, n int) client.Session {
	return client.Session{ID: id, Name: "s-" + id, State: "live", Status: "running", Attachable: true, Started: runT0.Add(time.Duration(n) * time.Minute)}
}

// runAttach runs an attach in the background and gives its Result on the
// channel.
func runAttach(ctx context.Context, term *runTerm, src source, id string, o Options) <-chan Result {
	done := make(chan Result, 1)
	go func() { done <- run(ctx, term, src, id, o) }()
	return done
}

// runWait waits for cond, and fails the test after a while without it.
func runWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// runResult waits for the attach to end.
func runResult(t *testing.T, done <-chan Result) Result {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the attach did not end")
	}
	return Result{}
}

// PutBack is what every end writes last, and every rescue from a panic,
// for a window rows high: every mode off and the main screen, then the
// attributes and the scroll region reset, which leaving the alternate
// screen would otherwise undo, and the cursor on the window's last row,
// where the caller's line goes. A window of no rows is put on its first.
// These are the only bytes of it any test spells out.
func TestPutBack(t *testing.T) {
	for rows, cup := range map[int]string{30: "\x1b[30;1H", 1: "\x1b[1;1H", 0: "\x1b[1;1H", -3: "\x1b[1;1H"} {
		if got, want := string(PutBack(rows)), string(vtscan.ResetAll())+"\x1b[0m\x1b[r"+cup; got != want {
			t.Errorf("PutBack(%d) = %q, want %q", rows, got, want)
		}
	}
	// Each call is a slice of its own, which a caller may append to.
	a := append(PutBack(24), "\r\n"...)
	if b := PutBack(24); strings.HasSuffix(string(b), "\r\n") || len(a) != len(b)+2 {
		t.Errorf("an append to one PutBack reached the next: %q", b)
	}
}

// runReset is what every end writes last, for a window rows high.
func runReset(rows int) string { return string(PutBack(rows)) }

func TestDetachDrainsResetsAndSaysSo(t *testing.T) {
	runKnobs(t)
	runSet(t, &drainFor, 300*time.Millisecond)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1cd")
	// What the session draws during the drain still reaches the screen, and
	// what is typed then reaches nobody.
	l.out("bye")
	term.typ(t, "late")
	r := runResult(t, done)
	if r.Why != Detached {
		t.Fatalf("Why = %v, want Detached (%v)", r.Why, r.Err)
	}
	if r.Session.ID != "a" || r.Session.Name != "s-a" {
		t.Errorf("Session = %+v, want a, named s-a", r.Session)
	}
	out := term.output()
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Errorf("the window was not cleared first: %q", out)
	}
	if !strings.HasSuffix(out, runReset(24)) {
		t.Errorf("output does not end with the reset: %q", out)
	}
	if i, j := strings.Index(out, "bye"), strings.LastIndex(out, runReset(24)); i < 0 || i > j {
		t.Errorf("the drain's output is not before the reset: %q", out)
	}
	if got := l.gotTyped(); got != "" {
		t.Errorf("the session was typed %q", got)
	}
	if !l.isClosed() {
		t.Error("the link was left open")
	}
}

func TestTypingReachesTheSession(t *testing.T) {
	for _, c := range []struct {
		name  string
		key   byte
		typed string
		want  string
		leave string
	}{
		{"default key", 0, "abc\x1c\x1c", "abc\x1c", "\x1cd"},
		// Under another key, Ctrl-\ is a key like any other.
		{"Ctrl-]", 0x1d, "x\x1cy\x1d\x1d", "x\x1cy\x1d", "\x1dd"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			done := runAttach(context.Background(), term, src, "a", Options{Key: c.key})
			term.typ(t, c.typed)
			runWait(t, "the typing", func() bool { return l.gotTyped() == c.want })
			term.typ(t, c.leave)
			if r := runResult(t, done); r.Why != Detached {
				t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
			}
			if got := l.gotTyped(); got != c.want {
				t.Errorf("typed %q, want %q", got, c.want)
			}
		})
	}
}

func TestWatchTypesNothing(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	done := runAttach(context.Background(), term, src, "a", Options{Watch: true})
	term.typ(t, "abc")
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
	}
	if got := l.gotTyped(); got != "" {
		t.Errorf("a watcher typed %q", got)
	}
	if w := src.gotWatched(); len(w) != 1 || !w[0] {
		t.Errorf("opened watching %v, want [true]", w)
	}
}

// TestABlockedSendLeavesTheLoopFree: typing into a CLI that is not reading
// stalls the send, never the attach: output is still drawn and d still
// leaves at once, and what was queued behind the stall is abandoned.
func TestABlockedSendLeavesTheLoopFree(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := runNewLink("a", 80, 24)
	l.block = make(chan struct{})
	src.add(l)
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "abc")
	term.typ(t, "def")
	l.out("still drawn")
	runWait(t, "output while the send waits", func() bool { return strings.Contains(term.output(), "still drawn") })
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
	}
	close(l.block)
	time.Sleep(20 * time.Millisecond)
	if got := l.gotTyped(); got != "" {
		t.Errorf("typed %q after the attach ended, want nothing", got)
	}
}

// TestCallsReachTheSessionInOrder: whatever is asked of a session while a
// send to it is stalled reaches it in the order it was asked, once the
// stall ends: the typing, then the claim, then the fit.
func TestCallsReachTheSessionInOrder(t *testing.T) {
	runKnobs(t)
	// Smaller than the session, so z fits it.
	term := runNewTerm(60, 20)
	src := runNewSrc(runSession("a", 0))
	l := runNewLink("a", 80, 24)
	l.block = make(chan struct{})
	src.add(l)
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "abc")
	term.typ(t, "def")
	term.typ(t, "\x1ck")
	term.typ(t, "\x1cz")
	time.Sleep(20 * time.Millisecond)
	close(l.block)
	runWait(t, "the calls", func() bool { return len(l.gotCalls()) == 4 })
	want := []string{"Type:abc", "Type:def", "Claim:false", "Fit:60x20"}
	if got := l.gotCalls(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("calls %q, want %q", got, want)
	}
	if got := l.gotTyped(); got != "abcdef" {
		t.Errorf("typed %q, want abcdef", got)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)
}

// TestNothingIsReadAfterRun: once Run has returned it receives nothing, so
// the next key is the next reader's.
func TestNothingIsReadAfterRun(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	src.add(runNewLink("a", 80, 24))
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "\x1cd")
	runResult(t, done)
	select {
	case term.in <- []byte("x"):
		t.Error("a key typed after Run returned was taken")
	case <-time.After(50 * time.Millisecond):
	}
}
