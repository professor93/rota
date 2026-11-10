//go:build linux || darwin

package hold

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/professor93/rota/internal/holdwire"
)

// A retired screen is kept again once it can be: a fresh emulator, after a
// quiet moment, at a size within the budget, for every retirement but a
// panic.

// renewing makes the sessions the test starts keep a retired screen again
// once their CLI has been quiet for d.
func renewing(t *testing.T, d time.Duration) {
	was := renewAfter
	renewAfter = d
	t.Cleanup(func() { renewAfter = was })
}

// step is what the emulator does when it wakes, on a session with nothing
// running: catch up, then keep a retired screen again if it can. It says
// how long until it may, as renew does.
func step(s *Session) time.Duration {
	s.em.lock.lock()
	defer s.em.lock.unlock()
	s.catchUp(false)
	return s.renew()
}

// longQuiet makes the CLI's last output long ago.
func longQuiet(s *Session) {
	s.mu.Lock()
	s.lastOut = time.Now().Add(-time.Hour)
	s.mu.Unlock()
}

func retired(s *Session) bool {
	s.em.lock.lock()
	defer s.em.lock.unlock()
	return s.em.retired
}

func fitTo(t *testing.T, s *Session, cols, rows uint16) {
	t.Helper()
	if _, ok := s.resize(holdwire.SizeMsg{Cols: cols, Rows: rows}); !ok {
		t.Fatalf("resizing to %dx%d was not done", cols, rows)
	}
}

// A screen retired for a size past the budget is kept again once the
// terminal fits and the CLI has been quiet for a moment. The fresh screen
// starts where the terminal took the size that fits, so what the CLI drew
// in answer to it is there, and what it printed at the size before is not.
// Until then, and while the size is past the budget, it stays retired.
func TestARetiredScreenIsKeptAgainOnceItFitsAndIsQuiet(t *testing.T) {
	s := newSession(Config{ID: testID}, newFakeChild()) // no emulator running
	defer func() { s.screenNow().Close() }()
	s.took([]byte("before\r\n"))
	fitTo(t, s, 500, 401)
	s.took([]byte("while too big\r\n"))
	longQuiet(s)
	if wait := step(s); wait != 0 || !retired(s) {
		t.Fatalf("past the budget: renew waits %v, retired %v", wait, retired(s))
	}
	fitTo(t, s, 80, 24)
	s.took([]byte("redrawn\r\n"))
	if wait := step(s); wait <= 0 || !retired(s) {
		t.Fatalf("within the budget, before a quiet moment: renew waits %v, retired %v", wait, retired(s))
	}
	longQuiet(s)
	if wait := step(s); wait != 0 || retired(s) {
		t.Fatalf("within the budget and quiet: renew waits %v, retired %v", wait, retired(s))
	}
	h, repaint, err := holdwire.ScreenOf(s.repaint(1))
	end := int64(len("before\r\n") + len("while too big\r\n") + len("redrawn\r\n"))
	if err != nil || !h.OK || h.Cols != 80 || h.Rows != 24 || h.Offset != end {
		t.Fatalf("the screen kept again answered %+v (%v)", h, err)
	}
	if got := string(repaint); !strings.Contains(got, "redrawn") || strings.Contains(got, "too big") || strings.Contains(got, "before") {
		t.Fatalf("the screen kept again shows %q", got)
	}
}

// A screen kept again draws no more of what came before than the emulator
// draws in a moment at its size, and from the start of a line, so it never
// begins inside a sequence it cannot read: one that fell a whole ring
// behind starts again on the last lines of the ring.
func TestARenewedScreenStartsOnALineOfTheTail(t *testing.T) {
	for _, c := range []struct {
		cols, rows uint16
		scrollback int
		flood      int
	}{
		{80, 24, 1024, 4096},          // the ring is the bound
		{200, 60, 64 << 10, 96 << 10}, // the moment's drawing is
	} {
		t.Run(fmt.Sprintf("%dx%d", c.cols, c.rows), func(t *testing.T) {
			s := newSession(Config{ID: testID, Cols: c.cols, Rows: c.rows, Scrollback: c.scrollback}, newFakeChild())
			defer func() { s.screenNow().Close() }()
			var b strings.Builder
			for i := 0; b.Len() < c.flood; i++ {
				fmt.Fprintf(&b, "line %06d %s\r\n", i, strings.Repeat("y", 40))
			}
			flood := []byte(b.String())
			for p := flood; len(p) > 0; {
				n := min(len(p), pumpRead)
				s.took(p[:n])
				p = p[n:]
			}
			if step(s); !retired(s) {
				t.Fatal("a screen a whole ring behind was not retired")
			}
			longQuiet(s)
			s.em.lock.lock()
			wait := s.renew()
			start, limit := s.em.off, drawLimit(int64(c.cols)*int64(c.rows))
			s.em.lock.unlock()
			end := int64(len(flood))
			if wait != 0 || retired(s) {
				t.Fatalf("renew waits %v, retired %v", wait, retired(s))
			}
			s.mu.Lock()
			oldest := s.ring.Oldest()
			s.mu.Unlock()
			if start < oldest || end-start > limit || start == end || flood[start-1] != '\n' {
				t.Fatalf("the screen kept again starts at %d of %d (oldest %d, limit %d), after %q",
					start, end, oldest, limit, flood[max(start-8, 0):start])
			}
			h, repaint, err := holdwire.ScreenOf(s.repaint(1))
			if err != nil || !h.OK || h.Offset != end || !bytes.Contains(repaint, flood[len(flood)-54:len(flood)-2]) {
				t.Fatalf("the screen kept again answered %+v with %q (%v)", h, repaint, err)
			}
		})
	}
}

// An emulator that panicked is never started again: what made it panic is
// the CLI's own output, which it would be fed again.
func TestAScreenWhoseEmulatorPanickedIsNotKeptAgain(t *testing.T) {
	orig := vtWrite
	t.Cleanup(func() { vtWrite = orig })
	vtWrite = func(em *vt.Emulator, p []byte) (int, error) {
		if bytes.Contains(p, []byte("boom")) {
			panic("x/vt fell over")
		}
		return orig(em, p)
	}
	s := newSession(Config{ID: testID}, newFakeChild()) // no emulator running
	defer func() { s.screenNow().Close() }()
	s.took([]byte("boom\r\n"))
	if step(s); !retired(s) {
		t.Fatal("a screen whose emulator panicked was not retired")
	}
	s.took([]byte("quiet now\r\n"))
	longQuiet(s)
	if wait := step(s); wait != 0 || !retired(s) || s.screenNow().OK() {
		t.Fatalf("after a quiet moment: renew waits %v, retired %v", wait, retired(s))
	}
}

// A retired screen comes back on its own: past the budget the session is
// replayed, and once it is fitted back and its CLI is quiet, a repaint is
// the CLI's screen again, from where it was fitted back, and the log says
// so once.
func TestARetiredScreenComesBackOnItsOwn(t *testing.T) {
	renewing(t, 50*time.Millisecond)
	r := newRig(t, 0, newFakeChild())
	claim(t, r.terminals)
	p := r.link(t)
	p.since(0)
	p.fit(500, 401)
	r.c.print(t, "while too big\r\n")
	if h, repaint := p.screen(1); h.OK || repaint != "" {
		t.Fatalf("past the budget the screen answered %+v with %d bytes", h, len(repaint))
	}
	p.fit(80, 24)
	r.c.print(t, "back\r\n")
	for seq, deadline := uint64(2), time.Now().Add(5*time.Second); ; seq++ {
		h, repaint := p.screen(seq)
		if h.OK {
			if h.Cols != 80 || h.Rows != 24 || !strings.Contains(repaint, "back") || strings.Contains(repaint, "too big") {
				t.Fatalf("the screen came back as %+v showing %q", h, repaint)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the screen never came back")
		}
		time.Sleep(20 * time.Millisecond)
	}
	n := 0
	for _, l := range r.logLines() {
		if strings.Contains(l, "kept again") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d lines about keeping the screen again: %q", n, r.logLines())
	}
}
