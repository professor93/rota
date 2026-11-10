//go:build linux || darwin

package hold

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/vtscan"
)

// The screen catches up on a read in one batch, written in as many pieces as
// it has cursor queries, and a batch of any number of pieces repeats no more
// than one write could: 32 KiB of a character, a REP of 99999999 and a cursor
// query, at 200×60, held the screen for 17 seconds when every piece had a
// budget of its own. What the budget leaves alone is drawn as x/vt draws it,
// every cursor query is answered where x/vt has the cursor, and the next
// batch has a budget of its own.
//
// Briefly is counted in the CPU time the process spends, not on the clock:
// on a loaded machine the batch waits for a CPU for as long as the machine
// makes it, and that wait is not the screen's doing. The 17 seconds were
// seconds of drawing.
func TestABatchOfRepeatsHoldsTheScreenBriefly(t *testing.T) {
	const w, h = 200, 60
	unit := "x\x1b[99999999b\x1b[6n"
	k := (32 << 10) / len(unit)
	in := strings.Repeat(unit, k)

	// The first REP draws two screens and the rest of 99999999 over the
	// width; every later one is past the batch's budget, and only its x is
	// drawn.
	want := unguarded(t, 80, 24, "")
	var answers strings.Builder
	func() {
		want.mu.Lock()
		defer want.mu.Unlock()
		want.em.Resize(w, h)
		for i := range k {
			piece := "x"
			if i == 0 {
				piece = fmt.Sprintf("x\x1b[%db", 2*w*h+99999999%w)
			}
			if _, err := want.em.Write([]byte(piece)); err != nil {
				t.Fatal(err)
			}
			at := want.em.CursorPosition()
			answers.Write(vtscan.CPRReply(at.Y+1, at.X+1, false))
		}
	}()

	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	r.c.reads()
	p := r.link(t)
	p.since(0)
	p.json(holdwire.Resize, holdwire.SizeMsg{Cols: w, Rows: h})
	p.decode(holdwire.Resized, &holdwire.SizeMsg{})

	// answered waits for the CLI to have read as much as want, and fails
	// where the two part.
	answered := func(want string) {
		t.Helper()
		for start := time.Now(); len(r.c.read()) < len(want) && time.Since(start) < 5*time.Second; {
			time.Sleep(time.Millisecond)
		}
		if got := r.c.read(); got != want {
			i := 0
			for i < min(len(got), len(want)) && got[i] == want[i] {
				i++
			}
			t.Fatalf("the CLI read %d bytes of answers, x/vt gives %d; they part at %d: %q, not %q",
				len(got), len(want), i, got[i:min(len(got), i+16)], want[i:min(len(want), i+16)])
		}
	}

	start, worked := time.Now(), busy(t)
	r.c.print(t, in)
	p.output(0, len(in))
	sh, _ := p.caughtUp(1, int64(len(in)))
	asked := busy(t) - worked
	answered(answers.String())
	if took, limit := busy(t)-worked, slow*200*time.Millisecond; asked > limit || took > limit || time.Since(start) > lingers {
		t.Errorf("%d bytes at %dx%d: the Screen came after %v of work, the cursor answers after %v (%v on the clock)",
			len(in), w, h, asked, took, time.Since(start))
	}
	if !sh.OK || sh.Offset != int64(len(in)) {
		t.Fatalf("Screen %+v after %d bytes", sh, len(in))
	}
	if d := screenDiff("x/vt", "screen", doc(t, want), doc(t, r.s.screen)); d != "" {
		t.Errorf("%d pieces of a REP between characters:\n%s", k, d)
	}

	// The next read is the next batch, with a budget of its own.
	want.mu.Lock()
	_, err := want.em.Write(fmt.Appendf(nil, "x\x1b[%db", 2*w*h+99999999%w))
	at := want.em.CursorPosition()
	want.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r.c.print(t, unit)
	answered(answers.String() + string(vtscan.CPRReply(at.Y+1, at.X+1, false)))
	if d := screenDiff("x/vt", "screen", doc(t, want), doc(t, r.s.screen)); d != "" {
		t.Errorf("a REP in the batch after:\n%s", d)
	}
}
