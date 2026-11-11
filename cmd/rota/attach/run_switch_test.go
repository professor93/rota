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
	"github.com/professor93/rota/internal/vtscan"
)

// n and p: moving between sessions.

func TestSwitchingCarriesNoKeys(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	a := src.add(runNewLink("a", 80, 24))
	b := src.add(runNewLink("b", 80, 24))
	gate := src.setGate("b")
	a.out("in a")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "a on the screen", func() bool { return strings.Contains(term.output(), "in a") })
	term.typ(t, "\x1cn")
	runWait(t, "b's open", func() bool { return len(src.gotBegun()) == 2 })
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
	if !a.isClosed() {
		t.Error("a was left open")
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
	// Listed in an order whose rotation is not the order they started in,
	// with one that is not attachable and one stopped, which n passes over.
	stopped := runSession("d", 3)
	stopped.State, stopped.Status = "stopped", "exited"
	odd := runSession("e", 4)
	odd.Attachable = false
	src := runNewSrc(runSession("a", 0), runSession("c", 2), stopped, odd, runSession("b", 1))
	for _, id := range []string{"a", "b", "c", "a", "c"} {
		src.add(runNewLink(id, 80, 24))
	}
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "a opened", func() bool { return len(src.gotOpens()) == 1 })
	for n := 2; n <= 4; n++ {
		term.typ(t, "\x1cn")
		runWait(t, "the next open", func() bool { return len(src.gotOpens()) == n })
	}
	if got := strings.Join(src.gotOpens(), " "); got != "a b c a" {
		t.Errorf("opened %s, want a b c a", got)
	}
	term.typ(t, "\x1cp")
	runWait(t, "c opened by p", func() bool { return len(src.gotOpens()) == 5 })
	if got := src.gotOpens()[4]; got != "c" {
		t.Errorf("p from a opened %s, want c", got)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)

	// Alone, n says so and stays.
	term2 := runNewTerm(80, 24)
	src2 := runNewSrc(runSession("a", 0))
	l := src2.add(runNewLink("a", 80, 24))
	done2 := runAttach(context.Background(), term2, src2, "a", Options{})
	term2.typ(t, "\x1cn")
	runWait(t, "the note", func() bool { return strings.Contains(term2.output(), "rota: no other live session") })
	term2.typ(t, "k")
	runWait(t, "typing goes on", func() bool { return l.gotTyped() == "k" })
	term2.typ(t, "\x1cd")
	if r := runResult(t, done2); r.Why != Detached || len(src2.gotOpens()) != 1 {
		t.Errorf("alone: %v after %v opens", r.Why, src2.gotOpens())
	}
}

// n and p never land on the session this terminal is inside, which would
// show it its own output: they pass over it both ways, and with only that
// one besides the session shown, n says there is no other.
func TestNextPassesOverTheSessionItIsIn(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0), runSession("b", 1), runSession("c", 2))
	for _, id := range []string{"a", "c", "a", "c"} {
		src.add(runNewLink(id, 80, 24))
	}
	done := runAttach(context.Background(), term, src, "a", Options{Inside: "b"})
	runWait(t, "a opened", func() bool { return len(src.gotOpens()) == 1 })
	for n, key := range []string{"\x1cn", "\x1cn", "\x1cp"} {
		term.typ(t, key)
		runWait(t, "the next open", func() bool { return len(src.gotOpens()) == n+2 })
	}
	if got := strings.Join(src.gotOpens(), " "); got != "a c a c" {
		t.Errorf("inside b, n n p opened %s, want a c a c", got)
	}
	term.typ(t, "\x1cd")
	runResult(t, done)

	term2 := runNewTerm(80, 24)
	src2 := runNewSrc(runSession("a", 0), runSession("b", 1))
	src2.add(runNewLink("a", 80, 24))
	done2 := runAttach(context.Background(), term2, src2, "a", Options{Inside: "b"})
	term2.typ(t, "\x1cn")
	runWait(t, "the note", func() bool { return strings.Contains(term2.output(), "rota: no other live session") })
	said := strings.Count(term2.output(), "rota: no other live session")
	term2.typ(t, "\x1cp")
	runWait(t, "the note again", func() bool { return strings.Count(term2.output(), "rota: no other live session") > said })
	term2.typ(t, "\x1cd")
	if r := runResult(t, done2); r.Why != Detached || len(src2.gotOpens()) != 1 {
		t.Errorf("inside b with only a besides: %v after %v opens", r.Why, src2.gotOpens())
	}
}

// TestAStalledSessionHoldsUpNoOther: a send to one session that stalls
// holds up neither n nor the next session's typing. What was asked of the
// stalled one still reaches it, in order, before its close.
func TestAStalledSessionHoldsUpNoOther(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	a := runNewLink("a", 80, 24)
	a.block = make(chan struct{})
	src.add(a)
	b := src.add(runNewLink("b", 80, 24))
	a.out("in a")
	b.out("in b")
	done := runAttach(context.Background(), term, src, "a", Options{})
	runWait(t, "a", func() bool { return strings.Contains(term.output(), "in a") })
	term.typ(t, "x")
	term.typ(t, "y")
	term.typ(t, "\x1ck")
	term.typ(t, "\x1cn")
	runWait(t, "b shown while a is stalled", func() bool { return strings.Contains(term.output(), "in b") })
	term.typ(t, "z")
	runWait(t, "z in b while a is stalled", func() bool { return b.gotTyped() == "z" })
	if got := a.gotCalls(); len(got) != 0 {
		t.Errorf("a was sent %q before its stall ended", got)
	}
	close(a.block)
	runWait(t, "a closed", a.isClosed)
	want := []string{"Type:x", "Type:y", "Claim:false", "Close"}
	if got := a.gotCalls(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("a's calls %q, want %q", got, want)
	}
	if a.late() || b.late() {
		t.Errorf("a closed link was typed into")
	}
	term.typ(t, "\x1cd")
	runResult(t, done)
}

// TestAListThatComesLateIsIgnored: the link lost while the list for n was
// under way is redialled, and the list that comes back afterwards switches
// nothing.
func TestAListThatComesLateIsIgnored(t *testing.T) {
	runKnobs(t)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	a1 := src.add(runNewLink("a", 80, 24))
	a2 := src.add(runNewLink("a", 80, 24))
	src.add(runNewLink("b", 80, 24))
	a1.out("one")
	a2.out("two")
	done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) { return nil, nil }})
	runWait(t, "one", func() bool { return strings.Contains(term.output(), "one") })
	// Every list from now waits, the one each open makes to name its
	// session among them, until the gate opens.
	gate := make(chan struct{})
	src.mu.Lock()
	src.listGate = gate
	src.mu.Unlock()
	lists := func() int {
		src.mu.Lock()
		defer src.mu.Unlock()
		return src.lists
	}
	before := lists()
	term.typ(t, "\x1cn")
	runWait(t, "the list for n", func() bool { return lists() > before })
	// The loop is not waiting on the list: the loss is taken at once, and
	// the redial is under way.
	a1.fail(io.ErrUnexpectedEOF)
	runWait(t, "the note", func() bool { return strings.Contains(term.output(), "went away") })
	runWait(t, "the redial's open", func() bool { return len(src.gotOpens()) == 2 })
	close(gate)
	runWait(t, "two", func() bool { return strings.Contains(term.output(), "two") })
	time.Sleep(50 * time.Millisecond)
	term.typ(t, "q")
	runWait(t, "q in a", func() bool { return a2.gotTyped() == "q" })
	for _, id := range src.gotBegun() {
		if id == "b" {
			t.Errorf("a list that came after the loss opened b: %v", src.gotBegun())
		}
	}
	term.typ(t, "\x1cd")
	if r := runResult(t, done); r.Why != Detached || r.Session.ID != "a" {
		t.Errorf("Result = %v on %q, want Detached on a", r.Why, r.Session.ID)
	}
}

func TestLeavingDuringASwitchLeaves(t *testing.T) {
	runKnobs(t)
	runSet(t, &drainFor, 100*time.Millisecond)
	term := runNewTerm(80, 24)
	src := runNewSrc(runSession("a", 0), runSession("b", 1))
	src.add(runNewLink("a", 80, 24))
	b := src.add(runNewLink("b", 80, 24))
	gate := src.setGate("b")
	b.out("in b")
	done := runAttach(context.Background(), term, src, "a", Options{})
	term.typ(t, "\x1cn")
	runWait(t, "b's open", func() bool { return len(src.gotBegun()) == 2 })
	term.typ(t, "\x1cd")
	// b's hello comes once the person has left: it is not shown.
	close(gate)
	r := runResult(t, done)
	if r.Why != Detached || r.Session.ID != "a" {
		t.Fatalf("Result = %v on %q (%v), want Detached on a", r.Why, r.Session.ID, r.Err)
	}
	runWait(t, "b closed", b.isClosed)
	if strings.Contains(term.output(), "in b") {
		t.Errorf("b was shown after the leave: %q", term.output())
	}
}

// TestAFailedSwitchKeepsTheSession: a session the daemon will not open is
// said in a note and the one shown stays; a daemon that cannot be reached
// is a lost link, and redialled.
func TestAFailedSwitchKeepsTheSession(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		src := runNewSrc(runSession("a", 0), runSession("b", 1))
		a := src.add(runNewLink("a", 80, 24))
		src.setRefuse("b", &client.Refusal{Status: 409, Message: "b is not attachable"})
		done := runAttach(context.Background(), term, src, "a", Options{})
		term.typ(t, "\x1cn")
		runWait(t, "the note", func() bool { return strings.Contains(term.output(), "rota: s-b: b is not attachable") })
		term.typ(t, "still a")
		runWait(t, "typing into a", func() bool { return a.gotTyped() == "still a" })
		term.typ(t, "\x1cd")
		if r := runResult(t, done); r.Why != Detached || r.Session.ID != "a" {
			t.Errorf("Result = %v on %q, want Detached on a", r.Why, r.Session.ID)
		}
		if a.isClosed() && a.late() {
			t.Error("a was typed into after closing")
		}
	})
	t.Run("daemon gone", func(t *testing.T) {
		runKnobs(t)
		term := runNewTerm(80, 24)
		src := runNewSrc(runSession("a", 0), runSession("b", 1))
		a1 := src.add(runNewLink("a", 80, 24))
		a2 := src.add(runNewLink("a", 80, 24))
		a2.out("a again")
		src.setRefuse("b", errors.New("the rota daemon at /x: dial unix /x: connection refused"))
		var mu sync.Mutex
		dials := 0
		done := runAttach(context.Background(), term, src, "a", Options{Redial: func() (*client.Client, error) {
			mu.Lock()
			defer mu.Unlock()
			dials++
			return nil, nil
		}})
		term.typ(t, "\x1cn")
		runWait(t, "a again", func() bool { return strings.Contains(term.output(), "a again") })
		runWait(t, "a's first link closed", a1.isClosed)
		term.typ(t, "\x1cd")
		if r := runResult(t, done); r.Why != Detached || r.Session.ID != "a" {
			t.Errorf("Result = %v on %q, want Detached on a", r.Why, r.Session.ID)
		}
		mu.Lock()
		defer mu.Unlock()
		if dials != 1 {
			t.Errorf("redialled %d times, want 1", dials)
		}
	})
}
