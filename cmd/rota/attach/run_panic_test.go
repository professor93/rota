package attach

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// runPanicLink is a scripted link that panics where it is told: Next on a
// frame of output "boom"; Type on anything with a "!" in it, once its
// runLink's block, when set, is closed; and Close when its source says so.
type runPanicLink struct {
	*runLink
	src *runPanicSrc
}

func (l *runPanicLink) Next() (client.Frame, error) {
	f, err := l.runLink.Next()
	if string(f.Output) == "boom" {
		panic("boom in Next")
	}
	return f, err
}

func (l *runPanicLink) Type(p []byte) error {
	if strings.Contains(string(p), "!") {
		if l.block != nil {
			<-l.block
		}
		panic("boom in Type")
	}
	return l.runLink.Type(p)
}

func (l *runPanicLink) Close() error {
	_ = l.runLink.Close()
	l.src.mu.Lock()
	boom := l.src.close_
	l.src.mu.Unlock()
	if boom {
		panic("boom in Close")
	}
	return nil
}

// runPanicSrc is a scripted daemon whose links panic as runPanicLink does,
// and which itself panics where it is told.
type runPanicSrc struct {
	*runSrc
	open_, list_, close_ bool // panic in open, in list, or in a link's Close
}

func (s *runPanicSrc) open(id string, watch bool) (link, error) {
	if s.open_ {
		panic("boom in open")
	}
	ln, err := s.runSrc.open(id, watch)
	if l, ok := ln.(*runLink); ok {
		return &runPanicLink{runLink: l, src: s}, err
	}
	return ln, err
}

// runLeft is how many goroutines are running a function whose name holds
// any of fns.
func runLeft(fns ...string) int {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	left := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, f := range fns {
			if strings.Contains(g, f) {
				left++
				break
			}
		}
	}
	return left
}

// runNoneLeft waits for no more goroutines running any of fns than there
// were before, and fails the test with the stacks after a while.
func runNoneLeft(t *testing.T, before int, fns ...string) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); runLeft(fns...) > before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines of the attach are left: %d running %v, %d before\n%s",
				runLeft(fns...), fns, before, buf[:runtime.Stack(buf, true)])
		}
	}
}

// runAttachFns are the functions an attach's goroutines run.
var runAttachFns = []string{"attach.(*linkWork).run", "attach.(*runner).next", "attach.(*runner).fail"}

func (s *runPanicSrc) list() ([]client.Session, error) {
	s.mu.Lock()
	boom := s.list_
	s.mu.Unlock()
	if boom {
		panic("boom in list")
	}
	return s.runSrc.list()
}

// A panic on any goroutine the attach starts besides its loop, in reading
// the link, opening a session, listing them for n, typing, or redialling,
// ends the attach Lost with the panic as its error, and the terminal is put
// back as on every other end: the process is not ended with the terminal
// raw and on the session's screen.
func TestAPanicOffTheLoopEndsTheAttach(t *testing.T) {
	for _, c := range []struct {
		name string
		o    Options
		do   func(t *testing.T, term *runTerm, l *runLink, s *runPanicSrc)
		want string
	}{
		{"reading the link", Options{}, func(t *testing.T, _ *runTerm, l *runLink, _ *runPanicSrc) {
			l.out("boom")
		}, "boom in Next"},
		{"typing", Options{}, func(t *testing.T, term *runTerm, _ *runLink, _ *runPanicSrc) {
			term.typ(t, "hi!")
		}, "boom in Type"},
		{"listing for n", Options{}, func(t *testing.T, term *runTerm, _ *runLink, s *runPanicSrc) {
			s.mu.Lock()
			s.list_ = true
			s.mu.Unlock()
			term.typ(t, "\x1cn")
		}, "boom in list"},
		{"redialling", Options{Redial: func() (*client.Client, error) { panic("boom in Redial") }},
			func(t *testing.T, _ *runTerm, l *runLink, _ *runPanicSrc) {
				l.fail(io.ErrUnexpectedEOF)
			}, "boom in Redial"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0), runSession("b", 1))}
			l := s.add(runNewLink("a", 80, 24))
			l.out("hi")
			done := runAttach(context.Background(), term, s, "a", c.o)
			runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
			c.do(t, term, l, s)
			r := runResult(t, done)
			if r.Why != Lost || r.Err == nil || !strings.Contains(r.Err.Error(), "attach: panic: "+c.want) {
				t.Fatalf("Result %v (%v), want Lost with the panic %q", r.Why, r.Err, c.want)
			}
			if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
				t.Errorf("output does not end with the reset: %q", out)
			}
			runWait(t, "the link closed", l.isClosed)
		})
	}

	// The first open panics: there is no session yet, and the end is the
	// same.
	runKnobs(t)
	term := runNewTerm(80, 24)
	s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0)), open_: true}
	r := runResult(t, runAttach(context.Background(), term, s, "a", Options{}))
	if r.Why != Lost || r.Err == nil || !strings.Contains(r.Err.Error(), "attach: panic: boom in open") {
		t.Fatalf("a panicking open: Result %v (%v), want Lost with the panic", r.Why, r.Err)
	}
	if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
		t.Errorf("output does not end with the reset: %q", out)
	}
}

// A panic on the attach's own loop, here in Teach, which the loop asks as
// it draws the first-attach card, goes on to Run's caller, but only after
// the attach has ended as every end does: the link closed, so the daemon
// lets go of the viewer, the reset written, and none of the attach's
// goroutines left behind, for a caller that recovers as a TUI does.
func TestAPanicOnTheLoopStillEndsTheAttach(t *testing.T) {
	runKnobs(t)
	before := runLeft(runAttachFns...)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	o := Options{Teach: func(string) bool { panic("boom in Teach") }}
	got := make(chan any, 1)
	go func() {
		defer func() { got <- recover() }()
		run(context.Background(), term, src, "a", o)
	}()
	select {
	case p := <-got:
		if p != "boom in Teach" {
			t.Fatalf("Run's caller got %v, want the panic boom in Teach", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attach did not end")
	}
	if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
		t.Errorf("output does not end with the reset: %q", out)
	}
	if !l.isClosed() {
		t.Error("the link was left open")
	}
	runNoneLeft(t, before, runAttachFns...)
}

// A link opened by the first open or by a redial, whose goroutine then
// panics before handing it to the loop, is closed: the loop never learns of
// it, and the daemon would keep its viewer for good.
func TestAnOpenedLinkIsClosedWhenItsOpenerPanics(t *testing.T) {
	t.Run("first open", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0)), list_: true}
		l := s.add(runNewLink("a", 80, 24))
		r := runResult(t, runAttach(context.Background(), term, s, "a", Options{}))
		if r.Why != Lost || r.Err == nil || !strings.Contains(r.Err.Error(), "boom in list") {
			t.Fatalf("Result %v (%v), want Lost with the panic", r.Why, r.Err)
		}
		runWait(t, "the opened link closed", l.isClosed)
	})
	t.Run("redial", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0))}
		l1 := s.add(runNewLink("a", 80, 24))
		l2 := s.add(runNewLink("a", 80, 24))
		l1.out("hi")
		done := runAttach(context.Background(), term, s, "a", Options{Redial: func() (*client.Client, error) { return nil, nil }})
		runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
		s.mu.Lock()
		s.list_ = true
		s.mu.Unlock()
		l1.fail(io.ErrUnexpectedEOF)
		r := runResult(t, done)
		if r.Why != Lost || r.Err == nil || !strings.Contains(r.Err.Error(), "boom in list") {
			t.Fatalf("Result %v (%v), want Lost with the panic", r.Why, r.Err)
		}
		runWait(t, "the redialled link closed", l2.isClosed)
	})
}

// A link whose Close panics as the attach ends changes nothing of the end,
// and does not end the process.
func TestAPanickingCloseAtTheEndIsTheEndsOwn(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0)), close_: true}
	l := s.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, s, "a", Options{})
	runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Result %v (%v), want Detached", r.Why, r.Err)
	}
	runWait(t, "the link closed", l.isClosed)
}

// A call that panics after the attach has ended, a send held up until
// then, is dropped, and leaves no goroutine waiting to hand it to a loop
// that is gone.
func TestALatePanicLeavesNothingBehind(t *testing.T) {
	runKnobs(t)
	before := runLeft(runAttachFns...)
	term := runNewTerm(80, 24)
	s := &runPanicSrc{runSrc: runNewSrc(runSession("a", 0))}
	l := s.add(runNewLink("a", 80, 24))
	l.block = make(chan struct{})
	l.out("hi")
	done := runAttach(context.Background(), term, s, "a", Options{})
	runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "x!") // held up, then panics
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Result %v (%v), want Detached", r.Why, r.Err)
	}
	close(l.block)
	runNoneLeft(t, before, runAttachFns...)
}
