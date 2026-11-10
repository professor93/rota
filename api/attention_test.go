package api

import (
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
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
// each attnBell: the bells in between are dropped, not saved up. A session
// whose registry speaks for it rings no bell (Review Focus 4).
func TestBellsAreRateLimited(t *testing.T) {
	attnRun(t, "three bells in thirty seconds", []attnStep{
		{in: attnIn{Now: attnAt(0), Bells: 1}, reason: "bell"},
		{in: attnIn{Now: attnAt(10 * time.Second), Bells: 2}},
		{in: attnIn{Now: attnAt(20 * time.Second), Bells: 3}},
		{in: attnIn{Now: attnAt(30 * time.Second), Bells: 3}},
		{in: attnIn{Now: attnAt(31 * time.Second), Bells: 4}, reason: "bell"},
		{in: attnIn{Now: attnAt(32 * time.Second), Bells: 4}},
	})
	attnRun(t, "bells beside a registry", []attnStep{
		{in: attnIn{Now: attnAt(0), Bells: 1, Registry: attnReg("working", "")}},
		{in: attnIn{Now: attnAt(time.Minute), Bells: 2, Registry: attnReg("working", "")}},
	})
}

// A title that goes from a working glyph to ✳, and stays there attnTitle,
// says the session is idle again. ✳ alone says nothing, nor does a title
// beside a registry that has said something of its own; one that has only
// ever said idle leaves the title to speak.
func TestTheTitleTurningIdle(t *testing.T) {
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
