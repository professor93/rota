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

// longQuiet makes the CLI's last output long ago. The sessions the unit
// tests make wait an hour for quiet, so they are quiet only once this says
// so, however slow the machine.
func longQuiet(s *Session) {
	s.mu.Lock()
	s.lastOut = time.Now().Add(-24 * time.Hour)
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
	s.renewAfter = time.Hour
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
			s.renewAfter = time.Hour
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
	s.renewAfter = time.Hour
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

// overtaken is a session with a ring of 1 KiB whose screen a flood of
// numbered lines has overtaken and retired, after what start printed.
func overtaken(t *testing.T, start string) *Session {
	t.Helper()
	s := newSession(Config{ID: testID, Scrollback: 1024}, newFakeChild()) // no emulator running
	s.renewAfter = time.Hour
	t.Cleanup(func() { s.screenNow().Close() })
	s.took([]byte(start))
	var b strings.Builder
	for i := 0; b.Len() < 4096; i++ {
		fmt.Fprintf(&b, "line %06d %s\r\n", i, strings.Repeat("y", 40))
	}
	s.took([]byte(b.String()))
	if step(s); !retired(s) {
		t.Fatal("a screen a whole ring behind was not retired")
	}
	return s
}

// kept says the screen was kept again on the next quiet moment, and what
// it repaints.
func kept(t *testing.T, s *Session) (holdwire.ScreenHead, string) {
	t.Helper()
	longQuiet(s)
	if wait := step(s); wait != 0 || retired(s) {
		t.Fatalf("after a quiet moment: renew waits %v, retired %v", wait, retired(s))
	}
	h, repaint, err := holdwire.ScreenOf(s.repaint(1))
	if err != nil || !h.OK {
		t.Fatalf("the screen kept again answered %+v (%v)", h, err)
	}
	return h, string(repaint)
}

// A fit to the size the session already has is no change of size: the CLI
// is sent no SIGWINCH and draws nothing, so the screen comes back on the
// last of the ring all the same. It came back empty from the fit on, where
// a replayed viewer saw the CLI.
func TestAFitToTheSameSizeIsNoChangeOfSize(t *testing.T) {
	s := overtaken(t, "")
	fitTo(t, s, 80, 24)
	if _, repaint := kept(t, s); !strings.Contains(repaint, "line ") {
		t.Fatalf("after a fit to the same size the screen came back showing %q", repaint)
	}
}

// A screen does not come back empty: after a size the CLI draws nothing
// for, it waits until the CLI prints, and then starts where the size was
// taken.
func TestAScreenWaitsForTheCLIToPrintAfterASize(t *testing.T) {
	s := overtaken(t, "")
	fitTo(t, s, 100, 30)
	longQuiet(s)
	if wait := step(s); wait != 0 || !retired(s) {
		t.Fatalf("with nothing printed at the new size: renew waits %v, retired %v", wait, retired(s))
	}
	s.took([]byte("hello\r\n"))
	if h, repaint := kept(t, s); h.Cols != 100 || h.Rows != 30 || !strings.Contains(repaint, "hello") || strings.Contains(repaint, "line ") {
		t.Fatalf("the screen came back as %+v showing %q", h, repaint)
	}
}

// A string the CLI has not finished, longer than the ring, leaves nowhere
// between two sequences to start on, and the screen stays retired: a
// repaint from inside it would have a viewer shown the rest as text.
func TestAScreenDoesNotComeBackInsideAString(t *testing.T) {
	s := overtaken(t, "")
	s.took([]byte("\x1b]52;c;" + strings.Repeat("QUJD", 1024)))
	longQuiet(s)
	if step(s); !retired(s) {
		t.Fatal("the screen came back inside a string")
	}
}

// A newline inside a string is not the start of a line: a fresh screen
// whose ring starts inside an OSC 52 wrapped at 76 columns starts at the ESC
// that ends it, and shows none of it.
func TestAScreenComesBackOnASequenceBeforeALine(t *testing.T) {
	s := overtaken(t, "")
	s.took([]byte("\x1b]52;c;" + strings.Repeat(strings.Repeat("QUJD", 19)+"\n", 30) + "\x1b\\after\r\n"))
	if _, repaint := kept(t, s); !strings.Contains(repaint, "after") || strings.Contains(repaint, "QUJD") {
		t.Fatalf("the screen came back showing %q", repaint)
	}
}

// A fresh screen starts past the last switch of screens it can see, so that
// it draws on the screen the CLI drew on: what the CLI drew on its
// alternate screen and then left is not on the main one.
func TestAScreenComesBackPastTheLastSwitchOfScreens(t *testing.T) {
	s := overtaken(t, "\x1b[?1049h")
	s.took([]byte("\x1b[1mALTLINE\x1b[0m\r\n\x1b[?1049lMAINLINE\r\n"))
	h, repaint := kept(t, s)
	if h.Modes.AltScreen || !strings.Contains(repaint, "MAINLINE") || strings.Contains(repaint, "ALTLINE") || strings.Contains(repaint, "line 0") {
		t.Fatalf("the screen came back as %+v showing %q", h, repaint)
	}
}

// A CLI on its alternate screen since before the ring is on it in the fresh
// screen too: when it leaves, the main screen it goes back to is not the one
// it drew on.
func TestAScreenComesBackOnTheAlternateScreen(t *testing.T) {
	s := overtaken(t, "\x1b[?1049h")
	if h, repaint := kept(t, s); !h.Modes.AltScreen || !strings.Contains(repaint, "line ") {
		t.Fatalf("the screen came back as %+v showing %q", h, repaint)
	}
	s.took([]byte("\x1b[?1049l"))
	step(s)
	h, repaint, err := holdwire.ScreenOf(s.repaint(2))
	if err != nil || !h.OK || h.Modes.AltScreen || strings.Contains(string(repaint), "line ") {
		t.Fatalf("back on the main screen it answered %+v showing %q (%v)", h, repaint, err)
	}
}

// A session that ends while a fresh screen is made keeps none: the fresh
// one is closed, and the screen life closes is the retired one.
func TestASessionEndingWhileAScreenIsMadeKeepsNone(t *testing.T) {
	s := overtaken(t, "")
	var made *Screen
	orig := newScreen
	t.Cleanup(func() { newScreen = orig })
	newScreen = func(cols, rows int) *Screen {
		close(s.finished)
		made = orig(cols, rows)
		return made
	}
	s.took([]byte("more\r\n"))
	longQuiet(s)
	if step(s); !retired(s) || made == nil || made.OK() || s.screenNow() == made {
		t.Fatalf("retired %v, made %v, its emulator alive %v", retired(s), made != nil, made != nil && made.OK())
	}
}
