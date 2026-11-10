package attach

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/professor93/rota/cmd/rota/client"
)

// runPanicLink is a scripted link that panics where it is told: Next on a
// frame of output "boom", and Type on anything with a "!" in it.
type runPanicLink struct{ *runLink }

func (l *runPanicLink) Next() (client.Frame, error) {
	f, err := l.runLink.Next()
	if string(f.Output) == "boom" {
		panic("boom in Next")
	}
	return f, err
}

func (l *runPanicLink) Type(p []byte) error {
	if strings.Contains(string(p), "!") {
		panic("boom in Type")
	}
	return l.runLink.Type(p)
}

// runPanicSrc is a scripted daemon whose links panic as runPanicLink does,
// and which itself panics where it is told.
type runPanicSrc struct {
	*runSrc
	open_, list_ bool // panic in open, or in list
}

func (s *runPanicSrc) open(id string, watch bool) (link, error) {
	if s.open_ {
		panic("boom in open")
	}
	ln, err := s.runSrc.open(id, watch)
	if l, ok := ln.(*runLink); ok {
		return &runPanicLink{l}, err
	}
	return ln, err
}

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
