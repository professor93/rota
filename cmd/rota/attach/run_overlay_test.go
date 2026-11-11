package attach

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/cmd/rota/client"
)

// What the attach draws over the session, and the chord keys that ask for
// it: notes, the hint, the cards, z and k, and the window changing.

func TestFitSendsThisWindow(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(fmt.Sprint("watch=", watch), func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(90, 20)
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
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("\x1b[31mhi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1c")
	runWait(t, "the hint", func() bool { return strings.Contains(term.output(), "d leave") })
	// Drawn between ESC 7 and ESC 8, so the CLI's pen comes back with the
	// cursor.
	after := term.runAfter("hi")
	if i, j, k := strings.Index(after, "\x1b7"), strings.Index(after, "d leave"), strings.Index(after, "\x1b8"); i < 0 || j < i || k < j {
		t.Errorf("the hint is not drawn between ESC 7 and ESC 8: %q", after)
	}
	term.typ(t, "d")
	if r := runResult(t, done); r.Why != Detached {
		t.Fatalf("Why = %v, want Detached", r.Why)
	}

	// An unknown key leaves the hint up for noteFor, then repaints.
	term2 := runNewTerm(80, 24)
	src2 := runNewSrc(runSession("a", 0))
	l2 := src2.add(runNewLink("a", 80, 24))
	l2.out("hi")
	done2 := runAttach(context.Background(), term2, src2, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term2.output(), "hi") })
	at := time.Now()
	term2.typ(t, "\x1cx")
	runWait(t, "the hint", func() bool { return strings.Contains(term2.output(), "d leave") })
	runWait(t, "the repaint", func() bool {
		a := term2.runAfter("d leave")
		return strings.Contains(a, "\x1b[H\x1b[2J") && strings.Contains(a, "hi")
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

// TestTheHintGoesWhenTheChordEnds: a chord key that acts takes the hint
// down at once, and the screen is repainted under it.
func TestTheHintGoesWhenTheChordEnds(t *testing.T) {
	runKnobs(t)
	runSet(t, &noteFor, 10*time.Second)
	// A window smaller than the session, so z fits it with no note of its
	// own to take the hint's place.
	term := runNewTerm(60, 20)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1c")
	runWait(t, "the hint", func() bool { return strings.Contains(term.output(), "d leave") })
	term.typ(t, "z")
	runWait(t, "the fit", func() bool { return len(l.gotFits()) == 1 })
	runWait(t, "the repaint", func() bool { return strings.Contains(term.runAfter("d leave"), "\x1b[H\x1b[2J") })
	term.typ(t, "\x1cd")
	runResult(t, done)
}

// TestAnOverlayWaitsForTheCLIsSequence: passing through, nothing the
// attach draws lands inside an escape sequence the CLI has begun.
func TestAnOverlayWaitsForTheCLIsSequence(t *testing.T) {
	runKnobs(t)
	runSet(t, &drainFor, 5*time.Second)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi\x1b[3")
	// Ended by a cancel: a d would drain for the drainFor set above.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAttach(ctx, term, src, "a", Options{})
	runWait(t, "the open sequence", func() bool { return strings.Contains(term.output(), "hi\x1b[3") })
	term.typ(t, "\x1c")
	time.Sleep(4 * hintAfter)
	if strings.Contains(term.output(), "d leave") {
		t.Fatalf("the hint was drawn inside the CLI's sequence: %q", term.output())
	}
	l.out("1m")
	runWait(t, "the hint", func() bool { return strings.Contains(term.output(), "d leave") })
	if !strings.Contains(term.output(), "hi\x1b[31m") {
		t.Errorf("the CLI's sequence was broken: %q", term.output())
	}
	cancel()
	runResult(t, done)
}

func TestTheChordIsTaughtOnce(t *testing.T) {
	for _, teach := range []bool{true, false} {
		t.Run(fmt.Sprint(teach), func(t *testing.T) {
			runKnobs(t)
			term := runNewTerm(80, 24)
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
				runWait(t, "the repaint", func() bool { return strings.Contains(term.runAfter("the session goes on"), "\x1b[H\x1b[2J") })
			} else {
				runWait(t, "Teach asked", func() bool {
					mu.Lock()
					defer mu.Unlock()
					return len(asked) == 1
				})
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

// TestTheTeachCardGoesByItself: with no key, the card goes after teachFor.
func TestTheTeachCardGoesByItself(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{Teach: func(string) bool { return true }})
	runWait(t, "the card", func() bool { return strings.Contains(term.output(), "then d; the session goes on") })
	at := time.Now()
	runWait(t, "the repaint", func() bool { return strings.Contains(term.runAfter("the session goes on"), "\x1b[H\x1b[2J") })
	if d := time.Since(at); d > teachFor+time.Second {
		t.Errorf("the card went after %v", d)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)
}

// TestTheTeachCardIsShownBeforeItCounts: typing that arrives with the
// attach, as a key after Settle's answer does, does not take the card down
// before it is drawn, and Teach, which records the person as taught, is
// asked only as the card is drawn.
func TestTheTeachCardIsShownBeforeItCounts(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	var mu sync.Mutex
	asked := 0
	o := Options{Settle: true, Teach: func(string) bool {
		mu.Lock()
		defer mu.Unlock()
		asked++
		return true
	}}
	done := runAttach(context.Background(), term, src, "a", o)
	term.typ(t, "\x1b[?62cq")
	runWait(t, "q typed", func() bool { return l.gotTyped() == "q" })
	runWait(t, "the card", func() bool { return strings.Contains(term.output(), "then d; the session goes on") })
	term.typ(t, "\x1cd")
	runResult(t, done)
	mu.Lock()
	defer mu.Unlock()
	if asked != 1 {
		t.Errorf("Teach asked %d times, want 1", asked)
	}
}

// TestTheHelpCardTakesItsOwnKey: the key that closes the ? card, Esc
// most often, is the card's, not the session's.
func TestTheHelpCardTakesItsOwnKey(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	l.out("hi")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "hi", func() bool { return strings.Contains(term.output(), "hi") })
	term.typ(t, "\x1c?")
	runWait(t, "the card", func() bool { return strings.Contains(term.output(), "rota: after Ctrl-\\, press") })
	term.typ(t, "\x1b")
	runWait(t, "the repaint", func() bool { return strings.Contains(term.runAfter("these keys"), "\x1b[H\x1b[2J") })
	time.Sleep(3 * holdFor)
	term.typ(t, "w")
	runWait(t, "w typed", func() bool { return l.gotTyped() != "" })
	if got := l.gotTyped(); got != "w" {
		t.Errorf("typed %q, want w alone: the Esc that closed the card reached the session", got)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)
}

// TestTheDaemonsWordsAreNotes: an error is said once while it repeats, and
// a request for the keyboard names who asks.
func TestTheDaemonsWordsAreNotes(t *testing.T) {
	runKnobs(t)
	runSet(t, &noteFor, 2*time.Second)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0))
	l := src.add(runNewLink("a", 80, 24))
	done := runAttach(context.Background(), term, src, "a", Options{})
	l.doc(client.Doc{Type: "error", Message: "bob holds the keyboard"})
	runWait(t, "the note", func() bool { return strings.Contains(term.output(), "rota: bob holds the keyboard") })
	l.doc(client.Doc{Type: "error", Message: "bob holds the keyboard"})
	l.doc(client.Doc{Type: "error", Message: "something else"})
	runWait(t, "the second note", func() bool { return strings.Contains(term.output(), "rota: something else") })
	if n := strings.Count(term.output(), "rota: bob holds the keyboard"); n != 1 {
		t.Errorf("the same error was said %d times within noteFor", n)
	}
	l.doc(client.Doc{Type: "claim_request", By: "carol"})
	runWait(t, "the claim note", func() bool {
		return strings.Contains(term.output(), "rota: carol asks for the keyboard; they may take it in 1s")
	})
	term.typ(t, "\x1cd")
	runResult(t, done)
}

func TestAWindowChangeNeverResizes(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
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
	runResult(t, done)
	if len(l.gotFits()) != 0 {
		t.Errorf("a window change fitted the session: %v", l.gotFits())
	}
	if !strings.HasSuffix(term.output(), runReset(40)) {
		t.Errorf("the reset is not for the window as it is: %q", term.output())
	}
}

// A terminal the server runs itself, rather than a held one, has no name:
// the crop's note calls it by its label, then its folder, then its id, as
// the daemon calls a held one, so a narrow window still has room for the
// sizes.
func TestAnUnnamedSessionIsCalledByItsLabel(t *testing.T) {
	for _, c := range []struct {
		s    client.Session
		want string
	}{
		{client.Session{ID: "0123456789abcdef", Label: "api", Cwd: "/w/web"}, "api is 100x30"},
		{client.Session{ID: "0123456789abcdef", Cwd: "/w/web"}, "web is 100x30"},
		{client.Session{ID: "0123456789abcdef"}, "0123456789abcdef is 100x30"},
		{client.Session{ID: "0123456789abcdef", Name: "named", Label: "api"}, "named is 100x30"},
	} {
		runKnobs(t)
		term := runNewTerm(60, 12)
		src := runNewSrc(c.s)
		l := src.add(runNewLink(c.s.ID, 100, 30))
		l.out("hi")
		done := runAttach(context.Background(), term, src, c.s.ID, Options{})
		runWait(t, c.want, func() bool { return strings.Contains(term.output(), "z fits; "+c.want) })
		term.typ(t, "\x1cd")
		runResult(t, done)
	}
}
