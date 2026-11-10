package attach

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
	"github.com/professor93/rota/internal/vtscan"
)

// runKnobs shortens every timing an attach waits on, for one test.
func runKnobs(t *testing.T) {
	t.Helper()
	set := func(p *time.Duration, d time.Duration) {
		old := *p
		*p = d
		t.Cleanup(func() { *p = old })
	}
	set(&drainFor, 20*time.Millisecond)
	set(&noteFor, 150*time.Millisecond)
	set(&teachFor, 300*time.Millisecond)
	set(&redialFor, 2*time.Second)
	set(&redialEvery, 10*time.Millisecond)
	set(&forceAfter, 200*time.Millisecond)
	set(&settleFor, 100*time.Millisecond)
	set(&hintAfter, 40*time.Millisecond)
	set(&cropEvery, 5*time.Millisecond)
}

// runTerm is the person's terminal: what the test types is read, and what
// the attach writes is kept.
type runTerm struct {
	inR *io.PipeReader
	inW *io.PipeWriter

	mu         sync.Mutex
	out        bytes.Buffer
	cols, rows int
	fail       error // the next write fails with it, once
	resized    chan struct{}
}

func runNewTerm(cols, rows int) *runTerm {
	r, w := io.Pipe()
	return &runTerm{inR: r, inW: w, cols: cols, rows: rows, resized: make(chan struct{}, 1)}
}

func (f *runTerm) Read(p []byte) (int, error) { return f.inR.Read(p) }

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

// typ types s, and returns once the attach has read it.
func (f *runTerm) typ(t *testing.T, s string) {
	t.Helper()
	wrote := make(chan error, 1)
	go func() {
		_, err := f.inW.Write([]byte(s))
		wrote <- err
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("typing %q: %v", s, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("typing %q: nobody read it", s)
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

	mu        sync.Mutex
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
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		l.typedLate = true
		return net.ErrClosed
	}
	l.typed = append(l.typed, p...)
	return nil
}

func (l *runLink) Fit(cols, rows uint16) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fits = append(l.fits, [2]uint16{cols, rows})
	return nil
}

func (l *runLink) Claim(force bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims = append(l.claims, force)
	return nil
}

func (l *runLink) Close() error {
	l.once.Do(func() {
		l.mu.Lock()
		l.closed = true
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
	opens    []string
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
	defer s.mu.Unlock()
	return append([]client.Session(nil), s.sessions...), nil
}

func (s *runSrc) setRefuse(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuse[id] = err
}

func (s *runSrc) gotOpens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opens...)
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

// runReset is what every end writes last, for a window rows high: the
// attributes and the scroll region reset, every mode off, the main screen,
// and the cursor on the window's last row, where the caller's line goes.
func runReset(rows int) string {
	return "\x1b[0m\x1b[r" + string(vtscan.ResetAll()) + fmt.Sprintf("\x1b[%d;1H", rows)
}

// runCleanup lets the attach's reader go once the test is over.
func runCleanup(t *testing.T, term *runTerm) {
	t.Cleanup(func() { term.inW.Close() })
}

func TestDetachDrainsResetsAndSaysSo(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1cd")
	// What the session draws during the drain still reaches the screen.
	l.out("bye")
	r := runResult(t, done)
	if r.Why != Detached {
		t.Fatalf("Why = %v, want Detached (%v)", r.Why, r.Err)
	}
	if r.Session.ID != "a" || r.Session.Name != "s-a" {
		t.Errorf("Session = %+v, want a, named s-a", r.Session)
	}
	out := term.output()
	if !strings.Contains(out, "hi") {
		t.Errorf("output %q lacks hi", out)
	}
	if !strings.HasSuffix(out, runReset(24)) {
		t.Errorf("output does not end with the reset: %q", out)
	}
	if got := l.gotTyped(); got != "" {
		t.Errorf("the session was typed %q", got)
	}
	if !strings.HasPrefix(out, "\x1b[H\x1b[2J") {
		t.Errorf("the window was not cleared first: %q", out)
	}
}

func TestEveryEndGivesTheTerminalBack(t *testing.T) {
	refused := &client.Refusal{Status: 409, Message: "a is not attachable"}
	cases := []struct {
		name string
		do   func(t *testing.T, term *runTerm, l *runLink, src *runSrc, cancel context.CancelFunc)
		why  Why
		code int
		err  error
	}{
		{"exit", func(t *testing.T, _ *runTerm, l *runLink, _ *runSrc, _ context.CancelFunc) {
			l.doc(client.Doc{Type: "exit", Code: 3})
		}, Ended, 3, nil},
		{"link lost", func(t *testing.T, _ *runTerm, l *runLink, _ *runSrc, _ context.CancelFunc) {
			l.fail(io.ErrUnexpectedEOF)
		}, Lost, 0, io.ErrUnexpectedEOF},
		{"cancelled", func(t *testing.T, _ *runTerm, _ *runLink, _ *runSrc, cancel context.CancelFunc) {
			cancel()
		}, Lost, 0, context.Canceled},
		{"write fails", func(t *testing.T, term *runTerm, l *runLink, _ *runSrc, _ context.CancelFunc) {
			term.mu.Lock()
			term.fail = errors.New("broken pipe")
			term.mu.Unlock()
			l.out("more")
		}, Lost, 0, nil},
		{"read fails", func(t *testing.T, term *runTerm, _ *runLink, _ *runSrc, _ context.CancelFunc) {
			term.inW.CloseWithError(io.EOF)
		}, Lost, 0, io.EOF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			runCleanup(t, term)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			l.out("hi")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runAttach(ctx, term, src, "a", Options{})
			runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
			c.do(t, term, l, src, cancel)
			r := runResult(t, done)
			if r.Why != c.why || r.Code != c.code {
				t.Fatalf("Result = %v %d (%v), want %v %d", r.Why, r.Code, r.Err, c.why, c.code)
			}
			if c.err != nil && !errors.Is(r.Err, c.err) {
				t.Errorf("Err = %v, want %v", r.Err, c.err)
			}
			if c.why == Lost && r.Err == nil {
				t.Errorf("a lost attach says nothing of why")
			}
			if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
				t.Errorf("output does not end with the reset: %q", out)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		runCleanup(t, term)
		src := runNewSrc(runSession("a", 0))
		src.setRefuse("a", refused)
		r := runResult(t, runAttach(context.Background(), term, src, "a", Options{}))
		if r.Why != Refused || !errors.Is(r.Err, refused) {
			t.Fatalf("Result = %v (%v), want Refused with the refusal", r.Why, r.Err)
		}
		if r.Session.Name != "s-a" {
			t.Errorf("a refused attach names %+v, want what the list knew of a", r.Session)
		}
		if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
			t.Errorf("output does not end with the reset: %q", out)
		}
	})
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
			runCleanup(t, term)
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
	runCleanup(t, term)
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
	src.mu.Lock()
	watched := src.watched
	src.mu.Unlock()
	if len(watched) != 1 || !watched[0] {
		t.Errorf("opened watching %v, want [true]", watched)
	}
}

func TestSwitchingCarriesNoKeys(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	a := src.add(runNewLink("a", 80, 24))
	b := src.add(runNewLink("b", 80, 24))
	gate := make(chan struct{})
	src.mu.Lock()
	src.gate["b"] = gate
	src.mu.Unlock()
	a.out("in a")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "a on the screen", func() bool { return strings.Contains(term.output(), "in a") })
	term.typ(t, "\x1cn")
	runWait(t, "a closed", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.closed
	})
	// Typed before b's hello: neither session gets it.
	term.typ(t, "xyz")
	close(gate)
	b.out("in b")
	runWait(t, "b on the screen", func() bool { return strings.Contains(term.output(), "in b") })
	term.typ(t, "q")
	runWait(t, "q in b", func() bool { return b.gotTyped() == "q" })
	term.typ(t, "\x1cd")
	r := runResult(t, done)
	if r.Why != Detached || r.Session.ID != "b" {
		t.Fatalf("Result = %v on %q, want Detached on b", r.Why, r.Session.ID)
	}
	if got := a.gotTyped(); got != "" {
		t.Errorf("a was typed %q", got)
	}
	if got := b.gotTyped(); got != "q" {
		t.Errorf("b was typed %q, want q", got)
	}
	if a.late() || b.late() {
		t.Errorf("a closed link was typed into")
	}
	// The switch put the window back before b was shown.
	out := term.output()
	if i, j := strings.Index(out, "in a"), strings.Index(out, "in b"); i < 0 || j < 0 || !strings.Contains(out[i:j], string(vtscan.ResetAll())+"\x1b[0m\x1b[r\x1b[H\x1b[2J") {
		t.Errorf("no reset and clear between a and b: %q", out)
	}
}

func TestNextWrapsAndSaysWhenAlone(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	// Listed out of order, and with one that is not attachable and one
	// stopped, which n passes over.
	stopped := runSession("d", 3)
	stopped.State, stopped.Status = "stopped", "exited"
	odd := runSession("e", 4)
	odd.Attachable = false
	src := runNewSrc(runSession("c", 2), stopped, runSession("a", 0), odd, runSession("b", 1))
	for _, id := range []string{"a", "b", "c", "a"} {
		l := src.add(runNewLink(id, 80, 24))
		l.out("in " + id + ";")
	}
	done := runAttach(context.Background(), term, src, "a", Options{})
	n := 1
	for _, want := range []string{"b", "c", "a"} {
		n++
		runWait(t, "the session before "+want, func() bool { return len(src.gotOpens()) == n-1 })
		term.typ(t, "\x1cn")
		runWait(t, want+" opened", func() bool { return len(src.gotOpens()) == n })
	}
	if got := strings.Join(src.gotOpens(), " "); got != "a b c a" {
		t.Errorf("opened %s, want a b c a", got)
	}
	term.typ(t, "\x1cp")
	runWait(t, "c opened by p", func() bool { return len(src.gotOpens()) == 5 })
	if got := src.gotOpens()[4]; got != "c" {
		t.Errorf("p from a opened %s, want c", got)
	}
	_ = src
	term.typ(t, "\x1cd")
	runResult(t, done)

	// Alone, n says so and stays.
	term2 := runNewTerm(80, 24)
	runCleanup(t, term2)
	src2 := runNewSrc(runSession("a", 0))
	src2.add(runNewLink("a", 80, 24))
	done2 := runAttach(context.Background(), term2, src2, "a", Options{})
	term2.typ(t, "\x1cn")
	runWait(t, "the note", func() bool { return strings.Contains(term2.output(), "rota: no other live session") })
	term2.typ(t, "\x1cd")
	if r := runResult(t, done2); r.Why != Detached || len(src2.gotOpens()) != 1 {
		t.Errorf("alone: %v after %v opens", r.Why, src2.gotOpens())
	}
}

func TestFitSendsThisWindow(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint("watch=", watch), func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(90, 20)
			runCleanup(t, term)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 100, 30))
			l.out("hi")
			done := runAttach(context.Background(), term, src, "a", Options{Watch: watch})
			runWait(t, "the crop", func() bool { return strings.Contains(term.output(), "z fits") })
			term.typ(t, "\x1cz")
			if watch {
				runWait(t, "the note", func() bool {
					return strings.Contains(term.output(), "rota: watching; this window cannot fit the session")
				})
			} else {
				runWait(t, "the fit", func() bool { return len(l.gotFits()) == 1 })
				if got := l.gotFits()[0]; got != [2]uint16{90, 20} {
					t.Errorf("Fit%v, want (90,20)", got)
				}
				// Once the session is this size, z says so.
				l.doc(client.Doc{Type: "resized", Cols: 90, Rows: 20})
				l.out("x")
				runWait(t, "out of the crop", func() bool { return strings.HasSuffix(term.output(), "x") })
				term.typ(t, "\x1cz")
				runWait(t, "the note", func() bool { return strings.Contains(term.output(), "rota: the session is already this size") })
			}
			term.typ(t, "\x1cd")
			runResult(t, done)
			if watch && len(l.gotFits()) != 0 {
				t.Errorf("a watcher fitted %v", l.gotFits())
			}
			if !watch && len(l.gotFits()) != 1 {
				t.Errorf("fits %v, want one", l.gotFits())
			}
		})
	}
}

func TestTheKeyboardIsAskedThenForced(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.hello.Holder, l.hello.HolderConn = "bob", "c9"
	done := runAttach(context.Background(), term, src, "a", Options{})
	start := time.Now()
	term.typ(t, "\x1ck")
	runWait(t, "the claim", func() bool { return len(l.gotClaims()) == 1 })
	if l.gotClaims()[0] {
		t.Errorf("the first k forced the claim")
	}
	l.doc(client.Doc{Type: "keyboard", Holder: "bob", HolderConn: "c9"})
	term.typ(t, "\x1ck")
	runWait(t, "the note", func() bool {
		return strings.Contains(term.output(), `rota: asked bob for the keyboard; Ctrl-\ k again after 1s takes it`)
	})
	if time.Since(start) < forceAfter && len(l.gotClaims()) != 1 {
		t.Errorf("a second k before forceAfter claimed: %v", l.gotClaims())
	}
	time.Sleep(forceAfter)
	term.typ(t, "\x1ck")
	runWait(t, "the forced claim", func() bool { return len(l.gotClaims()) == 2 })
	if !l.gotClaims()[1] {
		t.Errorf("k after forceAfter did not force: %v", l.gotClaims())
	}
	// Once this window holds it, k says so and asks nothing.
	l.doc(client.Doc{Type: "keyboard", Holder: "me", HolderConn: "c1"})
	l.out("x")
	runWait(t, "the keyboard doc", func() bool { return strings.HasSuffix(term.output(), "x") })
	term.typ(t, "\x1ck")
	runWait(t, "the note", func() bool { return strings.Contains(term.output(), "rota: this window holds the keyboard") })
	term.typ(t, "\x1cd")
	runResult(t, done)
	if n := len(l.gotClaims()); n != 2 {
		t.Errorf("%d claims, want 2", n)
	}
}

func TestTheHintRowAndItsRepair(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1c")
	runWait(t, "the hint", func() bool { return strings.Contains(term.output(), "d leave") })
	if out := term.output(); strings.Contains(out, "\x1b7") || strings.Contains(out, "\x1b8") {
		t.Errorf("an overlay used the CLI's own cursor save: %q", out)
	}
	term.typ(t, "d")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v, want Detached", r.Why)
	}

	// An unknown key leaves the hint up for noteFor, then repaints.
	term2 := runNewTerm(80, 24)
	runCleanup(t, term2)
	src2 := runNewSrc(runSession("a", 0))
	l2 := src2.add(runNewLink("a", 80, 24))
	l2.out("hi")
	done2 := runAttach(context.Background(), term2, src2, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term2.output(), "hi") })
	at := time.Now()
	term2.typ(t, "\x1cx")
	runWait(t, "the hint", func() bool { return strings.Contains(term2.output(), "d leave") })
	runWait(t, "the repaint", func() bool {
		out := term2.output()
		i := strings.LastIndex(out, "d leave")
		return strings.Contains(out[i:], "\x1b[H\x1b[2J") && strings.Contains(out[i:], "hi")
	})
	if d := time.Since(at); d < noteFor {
		t.Errorf("the hint went after %v, before noteFor", d)
	}
	if got := l2.gotTyped(); got != "" {
		t.Errorf("the unknown key was typed: %q", got)
	}
	term2.typ(t, "\x1cd")
	runResult(t, done2)
}

func TestTheChordIsTaughtOnce(t *testing.T) {
	for _, teach := range []bool{true, false} {
		t.Run(fmt.Sprint(teach), func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			runCleanup(t, term)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			l.out("hi")
			var asked []string
			var mu sync.Mutex
			o := Options{Teach: func(p string) bool {
				mu.Lock()
				defer mu.Unlock()
				asked = append(asked, p)
				return teach
			}}
			done := runAttach(context.Background(), term, src, "a", o)
			card := `rota: to leave, press Ctrl-\ then d; the session goes on`
			if teach {
				runWait(t, "the card", func() bool { return strings.Contains(term.output(), card) })
				// It goes at the first key, which is typed as usual.
				term.typ(t, "a")
				runWait(t, "a typed", func() bool { return l.gotTyped() == "a" })
				runWait(t, "the repaint", func() bool {
					out := term.output()
					return strings.Contains(out[strings.LastIndex(out, "the session goes on"):], "\x1b[H\x1b[2J")
				})
			} else {
				runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
				time.Sleep(20 * time.Millisecond)
			}
			term.typ(t, "\x1cd")
			runResult(t, done)
			if !teach && strings.Contains(term.output(), card) {
				t.Errorf("the card was shown to somebody taught")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != 1 || asked[0] != "me" {
				t.Errorf("Teach asked %v, want [me]", asked)
			}
		})
	}
}

func TestAWindowChangeNeverResizes(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
	if strings.Contains(term.output(), "\x1b[?1049h") {
		t.Fatalf("a window that holds the session was cropped")
	}
	term.resize(60, 20)
	runWait(t, "the crop", func() bool { return strings.Contains(term.output(), "\x1b[?1049h") })
	term.resize(120, 40)
	runWait(t, "out of the crop", func() bool { return strings.Contains(term.output(), "\x1b[?1049l") })
	term.typ(t, "\x1cd")
	r := runResult(t, done)
	if len(l.gotFits()) != 0 {
		t.Errorf("a window change fitted the session: %v", l.gotFits())
	}
	if !strings.HasSuffix(term.output(), runReset(40)) {
		t.Errorf("the reset is not for the window as it is: %q", term.output())
	}
	_ = r
}

func TestADaemonRestartIsAReconnect(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l1 := src.add(runNewLink("a", 80, 24))
	l2 := src.add(runNewLink("a", 80, 24))
	l2.hello.Repaint = true
	l2.out("REPAINTED")
	l1.out("first")
	var mu sync.Mutex
	tries := 0
	o := Options{Redial: func() (*client.Client, error) {
		mu.Lock()
		defer mu.Unlock()
		tries++
		if tries <= 2 {
			return nil, errors.New("no rota daemon is running")
		}
		return nil, nil
	}}
	done := runAttach(context.Background(), term, src, "a", o)
	runWait(t, "first", func() bool { return strings.Contains(term.output(), "first") })
	l1.fail(io.ErrUnexpectedEOF)
	runWait(t, "the note", func() bool {
		return strings.Contains(term.output(), `rota: the rota daemon went away; waiting for it (Ctrl-\ d leaves)`)
	})
	runWait(t, "the repaint", func() bool { return strings.Contains(term.output(), "REPAINTED") })
	mu.Lock()
	if tries != 3 {
		t.Errorf("redialled %d times, want 3", tries)
	}
	mu.Unlock()
	term.typ(t, "after")
	runWait(t, "typing in the new link", func() bool { return l2.gotTyped() == "after" })
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
	}
	if got := l1.gotTyped(); got != "" {
		t.Errorf("the lost link was typed %q", got)
	}
}

func TestNothingTypedWhileAwayIsSent(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l1 := src.add(runNewLink("a", 80, 24))
	l2 := src.add(runNewLink("a", 80, 24))
	l1.out("first")
	back := make(chan struct{})
	o := Options{Redial: func() (*client.Client, error) {
		select {
		case <-back:
			return nil, nil
		default:
			return nil, errors.New("not yet")
		}
	}}
	done := runAttach(context.Background(), term, src, "a", o)
	runWait(t, "first", func() bool { return strings.Contains(term.output(), "first") })
	l1.fail(io.ErrUnexpectedEOF)
	runWait(t, "the note", func() bool { return strings.Contains(term.output(), "went away") })
	term.typ(t, "lost keys")
	close(back)
	l2.out("back")
	runWait(t, "back", func() bool { return strings.Contains(term.output(), "back") })
	term.typ(t, "k2")
	runWait(t, "k2", func() bool { return l2.gotTyped() == "k2" })
	term.typ(t, "\x1cd")
	runResult(t, done)
	if got := l1.gotTyped() + l2.gotTyped(); got != "k2" {
		t.Errorf("typed %q, want only k2", got)
	}
}

func TestRedialGivesUp(t *testing.T) {
	runKnobs(t)
	redialFor = 100 * time.Millisecond
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("first")
	gone := errors.New("no rota daemon is running")
	done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) { return nil, gone }})
	runWait(t, "first", func() bool { return strings.Contains(term.output(), "first") })
	at := time.Now()
	l.fail(io.ErrUnexpectedEOF)
	r := runResult(t, done)
	if r.Why != Lost || !errors.Is(r.Err, gone) {
		t.Fatalf("Result = %v (%v), want Lost with the last failure", r.Why, r.Err)
	}
	if d := time.Since(at); d < redialFor {
		t.Errorf("gave up after %v, before redialFor", d)
	}
	if !strings.HasSuffix(term.output(), runReset(24)) {
		t.Errorf("output does not end with the reset: %q", term.output())
	}
}

func TestARedialToAStoppedSessionSaysItEnded(t *testing.T) {
	for _, c := range []struct {
		name string
		exit *int
		why  Why
		code int
	}{
		{"with its exit", func() *int { n := 7; return &n }(), Ended, 7},
		{"without one", nil, Ended, -1},
		{"still live", nil, Refused, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			runCleanup(t, term)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			l.out("first")
			refusal := &client.Refusal{Status: 409, Message: "a is not attachable"}
			done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) { return nil, nil }})
			runWait(t, "first", func() bool { return strings.Contains(term.output(), "first") })
			src.mu.Lock()
			if c.why == Ended {
				ended := runT0
				src.sessions[0].State, src.sessions[0].Ended, src.sessions[0].Exit = "stopped", &ended, c.exit
			}
			src.refuse["a"] = refusal
			src.mu.Unlock()
			l.fail(io.ErrUnexpectedEOF)
			r := runResult(t, done)
			if r.Why != c.why || r.Code != c.code {
				t.Fatalf("Result = %v %d (%v), want %v %d", r.Why, r.Code, r.Err, c.why, c.code)
			}
			if c.why == Refused && !errors.Is(r.Err, refusal) {
				t.Errorf("Err = %v, want the refusal", r.Err)
			}
		})
	}
}

func TestSettleDropsStaleReplies(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		runCleanup(t, term)
		src := runNewSrc(runSession("a", 0))
		l := src.add(runNewLink("a", 80, 24))
		done := runAttach(context.Background(), term, src, "a", Options{Settle: true})
		term.typ(t, "\x1b]11;rgb:0/0/0\x07\x1b[?62;22ca")
		runWait(t, "a typed", func() bool { return l.gotTyped() == "a" })
		term.typ(t, "\x1cd")
		runResult(t, done)
		if out := term.output(); !strings.HasPrefix(out, "\x1b[c\x1b[H\x1b[2J") {
			t.Errorf("output %q does not begin with the query and the clear", out)
		}
		if got := l.gotTyped(); got != "a" {
			t.Errorf("typed %q, want a", got)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		runCleanup(t, term)
		src := runNewSrc(runSession("a", 0))
		l := src.add(runNewLink("a", 80, 24))
		done := runAttach(context.Background(), term, src, "a", Options{Settle: true})
		term.typ(t, "\x1b]11;rgb:0/0/0\x07\x1b[1;2Rb")
		runWait(t, "the typing", func() bool { return l.gotTyped() == "\x1b[1;2Rb" })
		term.typ(t, "\x1cd")
		runResult(t, done)
	})
}

func TestTaughtFileRemembers(t *testing.T) {
	dir := t.TempDir()
	taught := TaughtFile(dir)
	if !taught("me") {
		t.Fatal("me was not to be taught the first time")
	}
	if taught("me") {
		t.Error("me was to be taught twice")
	}
	if !taught("bob") {
		t.Error("bob was not to be taught")
	}
	if TaughtFile(dir)("me") {
		t.Error("a second TaughtFile forgot me")
	}
	if TaughtFile(dir)("") {
		t.Error("a person with no name was to be taught")
	}
	path := filepath.Join(dir, "taught")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "me\nbob\n" {
		t.Errorf("the file holds %q, want me and bob, a line each", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Errorf("the file is %v, want 0600", fi.Mode().Perm())
	}
	// A folder it cannot write in teaches nobody.
	if TaughtFile(filepath.Join(dir, "missing"))("carol") {
		t.Error("a file that cannot be written said to teach")
	}
}

func TestLeavingDuringASwitchLeaves(t *testing.T) {
	runKnobs(t)
	drainFor = 100 * time.Millisecond
	term := runNewTerm(80, 24)
	runCleanup(t, term)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	src.add(runNewLink("a", 80, 24))
	b := src.add(runNewLink("b", 80, 24))
	gate := make(chan struct{})
	src.mu.Lock()
	src.gate["b"] = gate
	src.mu.Unlock()
	b.out("in b")
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "\x1cn")
	term.typ(t, "\x1cd")
	// b's hello comes once the person has left: it is not shown.
	time.Sleep(drainFor / 4)
	close(gate)
	r := runResult(t, done)
	if r.Why != Detached {
		t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
	}
	runWait(t, "b closed", func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.closed
	})
	if strings.Contains(term.output(), "in b") {
		t.Errorf("b was shown after the leave: %q", term.output())
	}
}
