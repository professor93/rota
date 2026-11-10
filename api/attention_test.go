package api

import (
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/ring"
	"github.com/professor93/rota/internal/share"
)

// The latches of a held session's attention, looked at with a clock that
// moves only as each test says: what a session that wants its person says,
// once, and what it never says.

// attnT0 is the moment every one of these tests starts from.
var attnT0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// attnAt is d after attnT0.
func attnAt(d time.Duration) time.Time { return attnT0.Add(d) }

// attnReg is a registry saying status, and waiting for waiting.
func attnReg(status, waiting string) *holdwire.Registry {
	return &holdwire.Registry{SessionID: "conv-1", Status: status, WaitingFor: waiting}
}

// attnStep is one look at a session and what it must say.
type attnStep struct {
	in     attnIn
	reason string
	quiet  bool
}

// attnRun takes the steps in order on one session's attention, and fails
// the test at the first that says other than it must.
func attnRun(t *testing.T, what string, steps []attnStep) {
	t.Helper()
	var a attention
	for i, st := range steps {
		reason, quiet := a.step(st.in)
		if reason != st.reason || quiet != st.quiet {
			t.Errorf("%s: look %d, at +%v, said (%q, %t); want (%q, %t)",
				what, i+1, st.in.Now.Sub(attnT0), reason, quiet, st.reason, st.quiet)
			return
		}
	}
}

// What the registry waits for is a permission when it says so, and a
// question otherwise; each is said once, and again only after it stopped.
func TestWaitingForIsPermissionOrQuestion(t *testing.T) {
	attnRun(t, "a permission, then a question", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(time.Second), Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(2 * time.Second), Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(3 * time.Second), Registry: attnReg("idle", "ask_user")}, reason: "question"},
		{in: attnIn{Now: attnAt(4 * time.Second), Registry: attnReg("idle", "ask_user")}},
	})
	attnRun(t, "a question that turns into a permission", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("idle", "waiting")}, reason: "question"},
		{in: attnIn{Now: attnAt(time.Second), Registry: attnReg("idle", "tool_permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(2 * time.Second), Registry: attnReg("idle", "tool_permission")}},
	})
}

// A registry that said its session was busy, whatever word it used, and
// then idle, says it has finished, once. One that was idle all along has
// finished nothing; one that is idle and asking says only that it asks.
func TestDoneIsBusyThenIdle(t *testing.T) {
	for _, busy := range []string{"busy", "working"} {
		attnRun(t, busy+" then idle", []attnStep{
			{in: attnIn{Now: attnAt(0), Registry: attnReg(busy, "")}},
			{in: attnIn{Now: attnAt(time.Second), Registry: attnReg(busy, "")}},
			{in: attnIn{Now: attnAt(2 * time.Second), Registry: attnReg("idle", "")}, reason: "done"},
			{in: attnIn{Now: attnAt(3 * time.Second), Registry: attnReg("idle", "")}},
			{in: attnIn{Now: attnAt(4 * time.Second), Registry: attnReg(busy, "")}},
			{in: attnIn{Now: attnAt(5 * time.Second), Registry: attnReg("idle", "")}, reason: "done"},
		})
	}
	attnRun(t, "idle from the start", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(time.Second), Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Registry: attnReg("idle", "")}},
	})
	attnRun(t, "working, then idle and asking", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Second), Registry: attnReg("idle", "waiting")}, reason: "question"},
		{in: attnIn{Now: attnAt(2 * time.Second), Registry: attnReg("idle", "waiting")}},
		{in: attnIn{Now: attnAt(3 * time.Second), Registry: attnReg("idle", "")}},
	})
}

// A CLI that rings its bell in a loop is said to have rung it at most once
// each attnBell. What rang in between is said once, as the window ends, so
// a second ring that meant something is late but never lost. A session
// whose registry has said anything of its own rings no bell (Review Focus
// 4).
func TestBellsAreRateLimited(t *testing.T) {
	attnRun(t, "three bells in thirty seconds", []attnStep{
		{in: attnIn{Now: attnAt(0), Bells: 1}, reason: "bell"},
		{in: attnIn{Now: attnAt(10 * time.Second), Bells: 2}},
		{in: attnIn{Now: attnAt(20 * time.Second), Bells: 3}},
		{in: attnIn{Now: attnAt(29 * time.Second), Bells: 3}},
		{in: attnIn{Now: attnAt(30 * time.Second), Bells: 3}, reason: "bell"},
		{in: attnIn{Now: attnAt(31 * time.Second), Bells: 3}},
		{in: attnIn{Now: attnAt(40 * time.Second), Bells: 4}},
		{in: attnIn{Now: attnAt(60 * time.Second), Bells: 4}, reason: "bell"},
		{in: attnIn{Now: attnAt(2 * time.Minute), Bells: 4}},
		{in: attnIn{Now: attnAt(3 * time.Minute), Bells: 5}, reason: "bell"},
	})
	attnRun(t, "a ring held back, and a registry that speaks before it is said", []attnStep{
		{in: attnIn{Now: attnAt(0), Bells: 1}, reason: "bell"},
		{in: attnIn{Now: attnAt(5 * time.Second), Bells: 2}},
		{in: attnIn{Now: attnAt(10 * time.Second), Bells: 2, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(30 * time.Second), Bells: 2, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Bells: 3, Registry: attnReg("working", "")}},
	})
	attnRun(t, "bells beside a registry", []attnStep{
		{in: attnIn{Now: attnAt(0), Bells: 1, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Bells: 2, Registry: attnReg("working", "")}},
	})
	attnRun(t, "bells beside a registry that has only asked", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(time.Second), Bells: 1, Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Bells: 2, Registry: attnReg("idle", "")}},
	})
}

// A title that goes from a working glyph to ✳, and stays there attnTitle,
// says the session is idle again. ✳ alone says nothing, nor does a glyph
// that is part of a word, nor a title beside a registry that has said a
// status other than idle. A registry that has only ever said idle leaves
// the title to speak, a wait or not: while it waits the title is that
// wait's, and the work after the answer is what arms it again.
func TestTheTitleTurningIdle(t *testing.T) {
	attnRun(t, "Claude Code 2.1.318: idle only, one permission, then finished", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(time.Second), Title: "⠂ Task", Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(2 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(10 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(11 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(15 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(16 * time.Second), Title: "⠂ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(20 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(22 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}, reason: "title"},
	})
	attnRun(t, "a title that works and rests while the registry waits", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task", Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(time.Second), Title: "⠂ Task", Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(2 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(time.Minute), Title: "✳ Task", Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(2 * time.Minute), Title: "✳ Task", Registry: attnReg("idle", "")}},
	})
	attnRun(t, "a glyph that is part of a word", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "·project"}},
		{in: attnIn{Now: attnAt(time.Second), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(time.Minute), Title: "✳ Task"}},
	})
	attnRun(t, "✳ that is part of a word", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task"}},
		{in: attnIn{Now: attnAt(time.Second), Title: "✳Task"}},
		{in: attnIn{Now: attnAt(time.Minute), Title: "✳Task"}},
	})
	for _, glyph := range []string{"⠂", "⠐", "✢", "✶", "✻", "✽", "·"} {
		attnRun(t, glyph+" then ✳", []attnStep{
			{in: attnIn{Now: attnAt(0), Title: glyph + " Task"}},
			{in: attnIn{Now: attnAt(time.Second), Title: "✳ Task"}},
			{in: attnIn{Now: attnAt(2 * time.Second), Title: "✳ Task"}},
			{in: attnIn{Now: attnAt(3 * time.Second), Title: "✳ Task"}, reason: "title"},
			{in: attnIn{Now: attnAt(4 * time.Second), Title: "✳ Task"}},
			{in: attnIn{Now: attnAt(time.Minute), Title: "✳ Task"}},
		})
	}
	attnRun(t, "✳ without a working glyph before", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(3 * time.Second), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(time.Minute), Title: "✳ Task"}},
	})
	attnRun(t, "✳ for less than attnTitle, then working again", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task"}},
		{in: attnIn{Now: attnAt(time.Second), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(2 * time.Second), Title: "⠂ Task"}},
		{in: attnIn{Now: attnAt(4 * time.Second), Title: "⠂ Task"}},
		{in: attnIn{Now: attnAt(5 * time.Second), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(6 * time.Second), Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(7 * time.Second), Title: "✳ Task"}, reason: "title"},
	})
	attnRun(t, "a registry that has only ever said idle", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(3 * time.Second), Title: "✳ Task", Registry: attnReg("idle", "")}, reason: "title"},
	})
	attnRun(t, "a registry that has said it was working", []attnStep{
		{in: attnIn{Now: attnAt(0), Title: "⠂ Task", Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Second), Title: "✳ Task", Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(3 * time.Second), Title: "✳ Task", Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Title: "✳ Task", Registry: attnReg("working", "")}},
	})
}

// heldQuiet after the last activity a session is quiet, once until it is
// active again. That is an attention of its own only for a session with
// neither a registry nor a title that speaks for it.
func TestQuietAfterActivity(t *testing.T) {
	act := attnAt(0)
	attnRun(t, "neither registry nor title", []attnStep{
		{in: attnIn{Now: attnAt(29 * time.Second), LastActive: act}},
		{in: attnIn{Now: attnAt(30 * time.Second), LastActive: act}, reason: "quiet", quiet: true},
		{in: attnIn{Now: attnAt(31 * time.Second), LastActive: act}},
		{in: attnIn{Now: attnAt(time.Hour), LastActive: act}},
		{in: attnIn{Now: attnAt(time.Hour + 29*time.Second), LastActive: attnAt(time.Hour)}},
		{in: attnIn{Now: attnAt(time.Hour + 30*time.Second), LastActive: attnAt(time.Hour)}, reason: "quiet", quiet: true},
	})
	attnRun(t, "a registry", []attnStep{
		{in: attnIn{Now: attnAt(29 * time.Second), LastActive: act, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(30 * time.Second), LastActive: act, Registry: attnReg("working", "")}, quiet: true},
		{in: attnIn{Now: attnAt(time.Hour), LastActive: act, Registry: attnReg("working", "")}},
	})
	attnRun(t, "a registry that has only asked", []attnStep{
		{in: attnIn{Now: attnAt(-time.Second), Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(29 * time.Second), LastActive: act, Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(30 * time.Second), LastActive: act, Registry: attnReg("idle", "")}, reason: "quiet", quiet: true},
	})
	attnRun(t, "a title that speaks", []attnStep{
		{in: attnIn{Now: attnAt(29 * time.Second), LastActive: act, Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(30 * time.Second), LastActive: act, Title: "✳ Task"}, quiet: true},
		{in: attnIn{Now: attnAt(time.Hour), LastActive: act, Title: "✳ Task"}},
	})
	attnRun(t, "no activity at all", []attnStep{
		{in: attnIn{Now: attnAt(time.Hour)}},
		{in: attnIn{Now: attnAt(2 * time.Hour)}},
	})
}

// While somebody has typed into the session in the last attnTyping, it
// says nothing of either kind, and loses nothing by it: what it would have
// said, it says once the typing is that long ago.
func TestTypingDefersAndDoesNotDrop(t *testing.T) {
	typed := attnAt(0)
	attnRun(t, "a permission", []attnStep{
		{in: attnIn{Now: attnAt(5 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(19 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(21 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}},
	})
	attnRun(t, "done", []attnStep{
		{in: attnIn{Now: attnAt(-time.Second), Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(2 * time.Second), TypedAt: typed, Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, Registry: attnReg("idle", "")}, reason: "done"},
	})
	attnRun(t, "a bell", []attnStep{
		{in: attnIn{Now: attnAt(time.Second), TypedAt: typed, Bells: 1}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, Bells: 1}, reason: "bell"},
	})
	attnRun(t, "a title", []attnStep{
		{in: attnIn{Now: attnAt(-5 * time.Second), Title: "⠂ Task"}},
		{in: attnIn{Now: attnAt(time.Second), TypedAt: typed, Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(10 * time.Second), TypedAt: typed, Title: "✳ Task"}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, Title: "✳ Task"}, reason: "title"},
	})
	attnRun(t, "quiet", []attnStep{
		{in: attnIn{Now: attnAt(10 * time.Second), TypedAt: typed, LastActive: attnAt(-20 * time.Second)}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, LastActive: attnAt(-20 * time.Second)}, reason: "quiet", quiet: true},
	})
	attnRun(t, "a permission that came and went while typing, and came back", []attnStep{
		{in: attnIn{Now: attnAt(-time.Second), Registry: attnReg("idle", "permission")}, reason: "permission"},
		{in: attnIn{Now: attnAt(2 * time.Second), TypedAt: typed, Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(4 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(20 * time.Second), TypedAt: typed, Registry: attnReg("idle", "permission")}, reason: "permission"},
	})
}

// What an adopted session is already doing when a daemon takes it up is
// where it starts from, so a restart re-announces nothing; what changes
// after that is said as for any session. A session started here has no
// such baseline: what its first look finds is news (Review Focus 4).
func TestAnAdoptedSessionStartsFromABaseline(t *testing.T) {
	attnRun(t, "an adopted session waiting for a permission", []attnStep{
		{in: attnIn{Now: attnAt(0), Adopted: true, Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(time.Minute), Adopted: true, Registry: attnReg("idle", "permission")}},
		{in: attnIn{Now: attnAt(2 * time.Minute), Adopted: true, Registry: attnReg("idle", "")}},
		{in: attnIn{Now: attnAt(3 * time.Minute), Adopted: true, Registry: attnReg("idle", "permission")}, reason: "permission"},
	})
	attnRun(t, "an adopted session that rang, is idle and has been quiet", []attnStep{
		{in: attnIn{Now: attnAt(0), Adopted: true, Bells: 7, Title: "✳ Task", LastActive: attnAt(-time.Hour)}},
		{in: attnIn{Now: attnAt(time.Second), Adopted: true, Bells: 7, Title: "✳ Task", LastActive: attnAt(-time.Hour)}},
		{in: attnIn{Now: attnAt(time.Minute), Adopted: true, Bells: 8, Title: "✳ Task", LastActive: attnAt(-time.Hour)}, reason: "bell"},
	})
	attnRun(t, "an adopted session at work", []attnStep{
		{in: attnIn{Now: attnAt(0), Adopted: true, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Second), Adopted: true, Registry: attnReg("idle", "")}, reason: "done"},
	})
	attnRun(t, "a session started here", []attnStep{
		{in: attnIn{Now: attnAt(0), Registry: attnReg("idle", "permission")}, reason: "permission"},
	})
}

// Output is not activity when it is the echo of a keystroke, the redraw
// after a fit, the CLI starting up, or what adoption replays; output a
// second after a keystroke is (Review Focus 4).
func TestEchoIsNotActivity(t *testing.T) {
	linked := attnAt(-time.Hour)
	for _, c := range []struct {
		what                   string
		keyAt, fitAt, linkedAt time.Time
		replayTo, off          int64
		at                     time.Time
		active                 bool
	}{
		{what: "output 30 ms after a key", linkedAt: linked, keyAt: attnT0, at: attnAt(30 * time.Millisecond)},
		{what: "output 1 s after a key", linkedAt: linked, keyAt: attnT0, at: attnAt(time.Second), active: true},
		{what: "output 1 s after a fit", linkedAt: linked, fitAt: attnT0, at: attnAt(time.Second)},
		{what: "output 3 s after a fit", linkedAt: linked, fitAt: attnT0, at: attnAt(3 * time.Second), active: true},
		{what: "output 5 s after the link", linkedAt: attnT0, at: attnAt(5 * time.Second)},
		{what: "output 11 s after the link", linkedAt: attnT0, at: attnAt(11 * time.Second), active: true},
		{what: "output adoption replays", linkedAt: linked, replayTo: 4096, off: 100, at: attnT0},
		{what: "output after what adoption replays", linkedAt: linked, replayTo: 4096, off: 4096, at: attnT0, active: true},
		{what: "output long after everything", linkedAt: linked, keyAt: linked, fitAt: linked, at: attnT0, active: true},
	} {
		h := &heldTerm{keyAt: c.keyAt, fitAt: c.fitAt, linkedAt: c.linkedAt, replayTo: c.replayTo}
		h.mu.Lock()
		h.noteOutput(c.off, c.at)
		got := h.activeAt
		h.mu.Unlock()
		if c.active && !got.Equal(c.at) {
			t.Errorf("%s left activeAt at %v, want %v", c.what, got, c.at)
		}
		if !c.active && !got.IsZero() {
			t.Errorf("%s was taken for activity at %v", c.what, got)
		}
	}

	// A session started here is linked from its CLI's first bytes, which
	// are its start and not activity.
	h := &heldTerm{}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.noteOutput(0, attnT0)
	if !h.linkedAt.Equal(attnT0) || !h.activeAt.IsZero() {
		t.Fatalf("the first output left linkedAt %v and activeAt %v", h.linkedAt, h.activeAt)
	}
	h.noteOutput(10, attnAt(5*time.Second))
	if !h.activeAt.IsZero() {
		t.Fatalf("output 5 s after the first was taken for activity")
	}
	h.noteOutput(20, attnAt(attnStart))
	if !h.activeAt.Equal(attnAt(attnStart)) {
		t.Fatalf("output attnStart after the first left activeAt at %v", h.activeAt)
	}
}

// What a terminal says by itself, focus in and out and the mouse, is not
// somebody typing: it never puts off what a session has to say. Anything
// else from a viewer is.
func TestFocusAndMouseReportsAreNotTyping(t *testing.T) {
	for _, c := range []struct {
		p       string
		reports bool
	}{
		{"\x1b[I", true},
		{"\x1b[O", true},
		{"\x1b[O\x1b[I", true},
		{"\x1b[M !!", true},
		{"\x1b[M#àá", true}, // UTF-8 coordinates (1005)
		{"\x1b[<0;10;5M", true},
		{"\x1b[<35;120;40m", true},
		{"\x1b[<0;10;5M\x1b[<0;10;5m\x1b[O", true},
		{"\x1b[32;10;5M", true}, // urxvt (1015)
		{"a", false},
		{"\x1b[A", false},
		{"\x1b", false},
		{"\x1b[I" + "a", false},
		{"\x1b[<0;10M", false},
		{"\x1b[M!", false},
		{"\x1b[200~text\x1b[201~", false},
		{"", false},
	} {
		if got := attnReportsOnly([]byte(c.p)); got != c.reports {
			t.Errorf("%q read as reports only: %t, want %t", c.p, got, c.reports)
		}
	}

	pp := attnPipe(t, false)
	tc := &termConn{}
	for _, p := range []string{"\x1b[I", "\x1b[<0;10;5M", "\x1b[O"} {
		pp.forward(time.Second)
		if err := pp.h.typeFrom(tc, []byte(p)); err != nil {
			t.Fatal(err)
		}
		if typed, key := pp.held(func(h *heldTerm) (time.Time, time.Time) { return h.typedAt, h.keyAt }); !typed.IsZero() || !key.Equal(pp.s.now()) {
			t.Fatalf("%q left typedAt %v and keyAt %v", p, typed, key)
		}
	}
	pp.forward(time.Second)
	if err := pp.h.typeFrom(tc, []byte("y")); err != nil {
		t.Fatal(err)
	}
	if typed, _ := pp.held(func(h *heldTerm) (time.Time, time.Time) { return h.typedAt, h.keyAt }); !typed.Equal(pp.s.now()) {
		t.Fatalf("a viewer typing y left typedAt at %v", typed)
	}
}

/* --------------------------------------------- a held session on a pipe --- */

// attnPiped is a held session on a bare server, linked over a pipe whose
// far end the test writes the holder's frames into. The server's clock is
// frozen at attnT0 and moves only by forward.
type attnPiped struct {
	t   *testing.T
	s   *Server
	h   *heldTerm
	ts  *termSession
	far *share.Conn
	mu  sync.Mutex
	at  time.Time
	off int64
}

// attnPipe makes one. Its link is read by heldTerm.read, as a holder's is,
// and whatever this end sends down it, keystrokes and pongs, is taken and
// thrown away at the far end.
func attnPipe(t *testing.T, adopted bool) *attnPiped {
	t.Helper()
	pp := &attnPiped{t: t, at: attnT0}
	s := &Server{terms: map[string]*termSession{}}
	s.clock.set(func() time.Time { pp.mu.Lock(); defer pp.mu.Unlock(); return pp.at })
	s.events = events.NewRing(64, s.now)
	near, far := net.Pipe()
	h := &heldTerm{s: s, conn: share.NewConn(near, 0), state: heldLive,
		asks: map[uint64]chan *heldScreen{}, left: make(chan struct{})}
	if adopted {
		// As adopt makes one: the record's guess at the registry, linked now.
		h.adopted, h.linkedAt = true, s.now()
		h.reg = holdwire.Registry{SessionID: "conv-1", Name: "secret plans"}
	}
	ts := &termSession{s: s, id: "0123456789abcdef", name: "piped", account: 2,
		back: h, cols: 80, rows: 24, ring: ring.New(1 << 16)}
	h.ts = ts
	s.terms[ts.id] = ts
	pp.s, pp.h, pp.ts, pp.far = s, h, ts, share.NewConn(far, 0)
	read := make(chan struct{})
	go func() {
		defer close(read)
		h.read(h.conn)
	}()
	go func() {
		for {
			if _, _, err := pp.far.Read(); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		pp.far.Close()
		h.conn.Close()
		<-read
	})
	return pp
}

func (pp *attnPiped) forward(d time.Duration) {
	pp.mu.Lock()
	pp.at = pp.at.Add(d)
	pp.mu.Unlock()
}

// held reads two of the session's times under its lock.
func (pp *attnPiped) held(read func(*heldTerm) (time.Time, time.Time)) (time.Time, time.Time) {
	pp.h.mu.Lock()
	defer pp.h.mu.Unlock()
	return read(pp.h)
}

// activeAt is the session's last activity.
func (pp *attnPiped) activeAt() time.Time {
	at, _ := pp.held(func(h *heldTerm) (time.Time, time.Time) { return h.activeAt, h.linkedAt })
	return at
}

// until waits for ok to hold, which takes whatever lock it reads under.
func (pp *attnPiped) until(what string, ok func() bool) {
	pp.t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			pp.t.Fatalf("the session never came to %s", what)
		}
	}
}

// print sends text as the holder's next output, and waits for the server
// to have taken it in.
func (pp *attnPiped) print(text string) {
	pp.t.Helper()
	if err := pp.far.Send(holdwire.Output, holdwire.PutOutput(pp.off, []byte(text))); err != nil {
		pp.t.Fatal(err)
	}
	pp.off += int64(len(text))
	pp.until("offset "+strconv.FormatInt(pp.off, 10), func() bool { return pp.ts.end() == pp.off })
}

// resize sends the holder's word that the CLI took a size, and waits for
// the server to have taken it in.
func (pp *attnPiped) resize(cols, rows uint16) {
	pp.t.Helper()
	if err := pp.far.SendJSON(holdwire.Resized, holdwire.SizeMsg{Cols: cols, Rows: rows}); err != nil {
		pp.t.Fatal(err)
	}
	pp.until("the new size", func() bool {
		pp.ts.mu.Lock()
		defer pp.ts.mu.Unlock()
		return pp.ts.cols == cols && pp.ts.rows == rows
	})
}

// state sends the holder's State, and waits for the server to have it.
func (pp *attnPiped) state(st holdwire.StateMsg) {
	pp.t.Helper()
	pp.h.mu.Lock()
	pp.h.stated = false // to see this one arrive
	pp.h.mu.Unlock()
	if err := pp.far.SendJSON(holdwire.State, st); err != nil {
		pp.t.Fatal(err)
	}
	pp.until("the state sent", func() bool {
		pp.h.mu.Lock()
		defer pp.h.mu.Unlock()
		return pp.h.stated
	})
}

// said is what the server published, as type and reason.
func (pp *attnPiped) said() []string {
	var out []string
	for _, e := range published(pp.s) {
		out = append(out, e.Type+" "+e.Reason)
	}
	return out
}

// The holder's frames, read as a link reads them and timed by the server's
// clock: output after the start is activity and heldQuiet later the session
// is said to have gone quiet; output right after a resize or a keystroke is
// the redraw or the echo, and is not activity.
func TestReadWeighsOutputOnTheServersClock(t *testing.T) {
	pp := attnPipe(t, false)
	pp.print("start")
	if at, linked := pp.held(func(h *heldTerm) (time.Time, time.Time) { return h.activeAt, h.linkedAt }); !at.IsZero() || !linked.Equal(attnT0) {
		t.Fatalf("the first output left activeAt %v and linkedAt %v", at, linked)
	}
	pp.forward(attnStart)
	pp.print("work")
	if at := pp.activeAt(); !at.Equal(pp.s.now()) {
		t.Fatalf("output attnStart after the first left activeAt at %v", at)
	}
	pp.forward(heldQuiet)
	pp.s.attendAll(pp.s.now())
	if got := pp.said(); len(got) != 2 || got[0] != "session.attention quiet" || got[1] != "session.quiet " {
		t.Fatalf("heldQuiet after its last activity the session said %v", got)
	}
	active := pp.activeAt()

	pp.forward(time.Minute)
	pp.resize(100, 30)
	pp.forward(time.Second)
	pp.print("redraw")
	if at := pp.activeAt(); !at.Equal(active) {
		t.Fatalf("output a second after a resize was taken for activity at %v", at)
	}
	pp.forward(attnFit)
	pp.print("more")
	if active = pp.activeAt(); !active.Equal(pp.s.now()) {
		t.Fatalf("output attnFit after a resize left activeAt at %v", active)
	}

	pp.forward(time.Second)
	if err := pp.h.typeFrom(nil, []byte("x")); err != nil {
		t.Fatal(err)
	}
	pp.print("x")
	if at := pp.activeAt(); !at.Equal(active) {
		t.Fatalf("the echo of a keystroke was taken for activity at %v", at)
	}
	pp.forward(time.Second)
	pp.print("answer")
	if at := pp.activeAt(); !at.Equal(pp.s.now()) {
		t.Fatalf("output a second after a keystroke left activeAt at %v", at)
	}
}

// A session a daemon took up is not looked at until its holder has said
// its state: its baseline is what the holder says, not the record's guess,
// so a wait under way when the daemon started is not said again (Review
// Focus 4).
func TestAnAdoptedSessionWaitsForItsHoldersState(t *testing.T) {
	pp := attnPipe(t, true)
	for range 3 {
		pp.s.attendAll(pp.s.now())
		pp.forward(time.Minute)
	}
	pp.h.mu.Lock()
	looked := pp.h.attn.looked
	pp.h.mu.Unlock()
	if looked || len(pp.said()) != 0 {
		t.Fatalf("a session whose holder has said nothing yet was looked at (%t) and said %v", looked, pp.said())
	}
	waiting := func(w string) holdwire.StateMsg {
		return holdwire.StateMsg{Title: "✳ x", Registry: &holdwire.Registry{SessionID: "conv-1", Status: "idle", WaitingFor: w}}
	}
	pp.state(waiting("permission"))
	pp.s.attendAll(pp.s.now())
	pp.forward(time.Minute)
	pp.s.attendAll(pp.s.now())
	if got := pp.said(); len(got) != 0 {
		t.Fatalf("the wait under way when it was taken up was said: %v", got)
	}
	pp.state(waiting(""))
	pp.s.attendAll(pp.s.now())
	pp.state(waiting("permission"))
	pp.s.attendAll(pp.s.now())
	got := published(pp.s, events.SessionAttention)
	if len(got) != 1 || got[0].Reason != "permission" || got[0].Name != "piped" || got[0].Account != 2 {
		t.Fatalf("a new wait was published as %+v", got)
	}
}
