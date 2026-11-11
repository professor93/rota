package attach

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// A lost daemon, and coming back to it.

func TestADaemonRestartIsAReconnect(t *testing.T) {
	for _, watch := range []bool{false, true} {
		name := "control"
		if watch {
			name = "watch"
		}
		t.Run(name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			src := runNewSrc(runSession("a", 0))
			l1 := src.add(runNewLink("a", 80, 24))
			l2 := src.add(runNewLink("a", 80, 24))
			l2.hello.Repaint = true
			l2.out("REPAINTED")
			l1.out("first")
			var mu sync.Mutex
			tries := 0
			o := Options{Watch: watch, Redial: func() (*client.Client, error) {
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
			runWait(t, "the lost link closed", l1.isClosed)
			mu.Lock()
			if tries != 3 {
				t.Errorf("redialled %d times, want 3", tries)
			}
			mu.Unlock()
			term.typ(t, "after")
			if !watch {
				runWait(t, "typing in the new link", func() bool { return l2.gotTyped() == "after" })
			}
			term.typ(t, "\x1cd")
			if r := runResult(t, done); r.Why != Detached {
				t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
			}
			if got := l1.gotTyped(); got != "" {
				t.Errorf("the lost link was typed %q", got)
			}
			for i, w := range src.gotWatched() {
				if w != watch {
					t.Errorf("open %d watching %v, want %v: a redial reopens as the attach began", i, w, watch)
				}
			}
			if watch && l2.gotTyped() != "" {
				t.Errorf("a watcher typed %q after the redial", l2.gotTyped())
			}
		})
	}
}

// TestASecondLossRedials: every loss of the daemon is redialled, not only
// the first.
func TestASecondLossRedials(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l1 := src.add(runNewLink("a", 80, 24))
	l2 := src.add(runNewLink("a", 80, 24))
	l3 := src.add(runNewLink("a", 80, 24))
	l1.out("ONE")
	l2.out("TWO")
	l3.out("THREE")
	done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) { return nil, nil }})
	runWait(t, "ONE", func() bool { return strings.Contains(term.output(), "ONE") })
	l1.fail(io.ErrUnexpectedEOF)
	runWait(t, "TWO", func() bool { return strings.Contains(term.output(), "TWO") })
	l2.fail(io.ErrUnexpectedEOF)
	runWait(t, "THREE", func() bool { return strings.Contains(term.output(), "THREE") })
	term.typ(t, "x")
	runWait(t, "typing in the third link", func() bool { return l3.gotTyped() == "x" })
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v (%v), want Detached", r.Why, r.Err)
	}
}

func TestNothingTypedWhileAwayIsSent(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
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

// TestAKeyBegunBeforeTheHelloIsDropped: an escape sequence the person began
// before the new link's hello is dropped whole, its tail with it, rather
// than its tail typed into the session.
func TestAKeyBegunBeforeTheHelloIsDropped(t *testing.T) {
	runKnobs(t)
	runSet(t, &holdFor, 5*time.Second)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l1 := src.add(runNewLink("a", 80, 24))
	l2 := src.add(runNewLink("a", 80, 24))
	l1.out("first")
	l2.out("back")
	gate := make(chan struct{})
	done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) {
		<-gate
		return nil, nil
	}})
	runWait(t, "first", func() bool { return strings.Contains(term.output(), "first") })
	l1.fail(io.ErrUnexpectedEOF)
	runWait(t, "the note", func() bool { return strings.Contains(term.output(), "went away") })
	term.typ(t, "\x1b[")
	close(gate)
	runWait(t, "back", func() bool { return strings.Contains(term.output(), "back") })
	term.typ(t, "Az")
	runWait(t, "z", func() bool { return strings.Contains(l2.gotTyped(), "z") })
	if got := l2.gotTyped(); got != "z" {
		t.Errorf("typed %q, want z alone", got)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)
}

func TestRedialGivesUp(t *testing.T) {
	runKnobs(t)
	runSet(t, &redialFor, 100*time.Millisecond)
	term := runNewTerm(80, 24)
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
