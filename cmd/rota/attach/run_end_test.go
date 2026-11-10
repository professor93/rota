package attach

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// How an attach ends, and what it gives back.

func TestEveryEndGivesTheTerminalBack(t *testing.T) {
	cases := []struct {
		name string
		do   func(t *testing.T, term *runTerm, l *runLink, cancel context.CancelFunc)
		why  Why
		code int
		err  error
	}{
		{"exit", func(t *testing.T, _ *runTerm, l *runLink, _ context.CancelFunc) {
			l.doc(client.Doc{Type: "exit", Code: 3})
		}, Ended, 3, nil},
		{"link lost", func(t *testing.T, _ *runTerm, l *runLink, _ context.CancelFunc) {
			l.fail(io.ErrUnexpectedEOF)
		}, Lost, 0, io.ErrUnexpectedEOF},
		{"cancelled", func(t *testing.T, _ *runTerm, _ *runLink, cancel context.CancelFunc) {
			cancel()
		}, Lost, 0, context.Canceled},
		{"write fails", func(t *testing.T, term *runTerm, l *runLink, _ context.CancelFunc) {
			term.mu.Lock()
			term.fail = errors.New("broken pipe")
			term.mu.Unlock()
			l.out("more")
		}, Lost, 0, nil},
		{"input ends", func(t *testing.T, term *runTerm, _ *runLink, _ context.CancelFunc) {
			close(term.in)
		}, Lost, 0, io.EOF},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			l.out("hi")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runAttach(ctx, term, src, "a", Options{})
			runWait(t, "hi on the screen", func() bool { return strings.Contains(term.output(), "hi") })
			c.do(t, term, l, cancel)
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
			if !l.isClosed() {
				t.Error("the link shown was left open")
			}
		})
	}
	for _, c := range []struct {
		name string
		err  error
	}{
		{"refused", &client.Refusal{Status: 409, Message: "a is not attachable"}},
		// Any first open that fails is an attach that never started.
		{"dial fails", errors.New("the rota daemon at /x: dial unix /x: connection refused")},
	} {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			src := runNewSrc(runSession("a", 0))
			src.setRefuse("a", c.err)
			r := runResult(t, runAttach(context.Background(), term, src, "a", Options{}))
			if r.Why != Refused || !errors.Is(r.Err, c.err) {
				t.Fatalf("Result = %v (%v), want Refused with %v", r.Why, r.Err, c.err)
			}
			if r.Session.Name != "s-a" {
				t.Errorf("a refused attach names %+v, want what the list knew of a", r.Session)
			}
			if out := term.output(); !strings.HasSuffix(out, runReset(24)) {
				t.Errorf("output does not end with the reset: %q", out)
			}
		})
	}
}

// TestTheEndLeavesNoPenBehind: leaving the alternate screen restores the
// cursor as ESC 8 does, with the pen saved beside it, so the attributes are
// reset after it: the caller's line is never in the session's colours.
func TestTheEndLeavesNoPenBehind(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	src.add(runNewLink("a", 80, 24))
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "\x1cd")
	runResult(t, done)
	out := term.output()
	if i, j := strings.LastIndex(out, "\x1b[?1049l"), strings.LastIndex(out, "\x1b[0m"); i < 0 || j < i {
		t.Errorf("the attributes are not reset after leaving the alternate screen: %q", out)
	}
}

func TestExitDuringTheDrainEnds(t *testing.T) {
	runKnobs(t)
	runSet(t, &drainFor, time.Second)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "\x1cd")
	l.doc(client.Doc{Type: "exit", Code: 4})
	if r := runResult(t, done); r.Why != Ended || r.Code != 4 {
		t.Errorf("Result = %v %d, want Ended 4: the session is gone, so nothing returns to it", r.Why, r.Code)
	}
}

func TestACancelDuringTheDrainIsADetach(t *testing.T) {
	runKnobs(t)
	runSet(t, &drainFor, time.Second)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	src.add(runNewLink("a", 80, 24))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAttach(ctx, term, src, "a", Options{})
	term.typ(t, "\x1cd")
	cancel()
	if r := runResult(t, done); r.Why != Detached {
		t.Errorf("Why = %v (%v), want Detached: the person had left", r.Why, r.Err)
	}
}

// TestWhatOpensAfterTheEndIsClosed: an open still under way when the
// attach ends is closed when it lands, first and for a switch alike.
func TestWhatOpensAfterTheEndIsClosed(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		src := runNewSrc(runSession("a", 0))
		a := src.add(runNewLink("a", 80, 24))
		gate := src.setGate("a")
		ctx, cancel := context.WithCancel(context.Background())
		done := runAttach(ctx, term, src, "a", Options{})
		runWait(t, "the open", func() bool { return len(src.gotBegun()) == 1 })
		cancel()
		if r := runResult(t, done); r.Why != Lost {
			t.Fatalf("Why = %v, want Lost", r.Why)
		}
		close(gate)
		runWait(t, "a closed", a.isClosed)
	})
	t.Run("switch", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		src := runNewSrc(runSession("a", 0), runSession("b", 1))
		a := src.add(runNewLink("a", 80, 24))
		b := src.add(runNewLink("b", 80, 24))
		gate := src.setGate("b")
		ctx, cancel := context.WithCancel(context.Background())
		done := runAttach(ctx, term, src, "a", Options{})
		term.typ(t, "\x1cn")
		runWait(t, "b's open", func() bool { return len(src.gotBegun()) == 2 })
		cancel()
		runResult(t, done)
		close(gate)
		runWait(t, "b closed", b.isClosed)
		runWait(t, "a closed", a.isClosed)
	})
}

func TestSettleDropsStaleReplies(t *testing.T) {
	for _, c := range []struct {
		name, typed, want string
	}{
		{"answered", "\x1b]11;rgb:0/0/0\x07\x1b[?62;22ca", "a"},
		// Whatever came before the answer was owed to whoever used the
		// terminal before, typing among it.
		{"typed before the answer", "x\x1b[?62ca", "a"},
		{"no answer", "\x1b]11;rgb:0/0/0\x07\x1b[1;2Rb", "\x1b[1;2Rb"},
	} {
		t.Run(c.name, func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
			src := runNewSrc(runSession("a", 0))
			l := src.add(runNewLink("a", 80, 24))
			done := runAttach(context.Background(), term, src, "a", Options{Settle: true})
			term.typ(t, c.typed)
			runWait(t, "the typing", func() bool { return l.gotTyped() == c.want })
			term.typ(t, "\x1cd")
			runResult(t, done)
			if out := term.output(); !strings.HasPrefix(out, "\x1b[c\x1b[H\x1b[2J") {
				t.Errorf("output %q does not begin with the query and the clear", out)
			}
			if got := l.gotTyped(); got != c.want {
				t.Errorf("typed %q, want %q", got, c.want)
			}
		})
	}
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
