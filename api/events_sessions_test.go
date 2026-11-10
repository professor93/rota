package api

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/ring"
	rota "github.com/professor93/rota/lib"
)

// What a session says about itself: that it started and ended, why and
// after how long, who attached to it and left it, and that the token it
// runs on ended. These are about sessions built in memory, whose far end
// does nothing; events_sessions_unix_test.go has the ones a CLI or a
// holder runs.

/* ------------------------------------------------------------ the harness --- */

// sessBack is a far end that does nothing, for a session whose ending the
// test calls itself.
type sessBack struct{}

func (sessBack) typeInto([]byte) error       { return nil }
func (sessBack) resize(uint16, uint16) error { return nil }
func (sessBack) hangUp()                     {}
func (sessBack) end()                        {}
func (sessBack) run(termSink) int            { return 0 }
func (sessBack) close()                      {}
func (sessBack) detach()                     {}
func (sessBack) describe(map[string]any)     {}

// sessIn is an account's session built in memory on s, started at at, in
// the folder app, on work@x.com's claude, with a far end that does nothing.
func sessIn(s *Server, at time.Time, tune ...func(*termSession)) *termSession {
	ts := &termSession{s: s, id: runID(), log: func(string, ...any) {},
		kind: termKindAccount, account: 1, label: "work@x.com", provider: "claude",
		cwd: "/x/app", started: at, back: sessBack{},
		cols: 80, rows: 24, ring: ring.New(1 << 16)}
	for _, f := range tune {
		f(ts)
	}
	return ts
}

// sessConn is one connection of name's, signed in by via, that nothing
// reads: what it is sent waits in its queue. It has already drained, so a
// session that ends does not wait for it to.
func sessConn(name, via string, role Role, watch bool) *termConn {
	tc := newTermConn(nil, &Principal{Name: name, Role: role, Via: via}, watch, time.Now())
	close(tc.drained)
	return tc
}

// sessClock stops s's clock at at, and gives what moves it on.
func sessClock(s *Server, at time.Time) (forward func(time.Duration)) {
	var mu sync.Mutex
	s.clock.set(func() time.Time { mu.Lock(); defer mu.Unlock(); return at })
	return func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }
}

// sessOf is every event s keeps of type typ about session id.
func sessOf(s *Server, typ, id string) []events.Event {
	var out []events.Event
	for _, e := range published(s, typ) {
		if e.Session == id {
			out = append(out, e)
		}
	}
	return out
}

// sessQuiet fails the test if s says anything of the given types about
// session id within d.
func sessQuiet(t *testing.T, s *Server, id string, d time.Duration, types ...string) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		for _, typ := range types {
			if got := sessOf(s, typ, id); len(got) > 0 {
				t.Fatalf("%s said %+v", id, got)
			}
		}
	}
}

// sessUTC is the time of day a token's end is said at.
func sessUTC(at time.Time) string { return at.UTC().Format("15:04") + " UTC" }

/* -------------------------------------------------------------- names --- */

// No event about a session names it after its conversation or its title:
// the name is what the person called it, else its folder, else its id, and
// the words name the account and the folder, never what the CLI said.
func TestSessionEventsNeverNameTheConversation(t *testing.T) {
	h := eventsServer(t)
	held := &heldTerm{convName: "secret-chat", title: "✳ secret-title", state: heldLive}
	ts := sessIn(h.api, time.Now(), func(ts *termSession) { ts.back = held })
	held.ts = ts
	h.api.sessionStarted(ts, "alice", "", "")
	ts.attach(sessConn("alice", "user", RoleControl, false), 0, true)
	ts.finish(0)
	all := published(h.api)
	if len(all) < 3 {
		t.Fatalf("a session that started, was attached to and ended said only %+v", all)
	}
	for _, e := range all {
		if e.Name != "app" || strings.Contains(e.Text, "secret") {
			t.Errorf("%s is named %q and says %q", e.Type, e.Name, e.Text)
		}
	}
}

/* ------------------------------------------------------------- started --- */

// session.started says who started it, on which account and in which
// folder; who is left out for nobody and for this machine's own user, and
// a resume, a move, a shell and a shared terminal each read as themselves.
func TestTheStartSaysWhoWhereAndWhy(t *testing.T) {
	h := eventsServer(t)
	for _, c := range []struct {
		what, by, why, text string
		tune                func(*termSession)
		want                string
	}{
		{"a start", "alice", "", "", nil, "started by alice on work@x.com (claude) in app"},
		{"nobody named", "", "", "", nil, "started on work@x.com (claude) in app"},
		{"this machine's user", "local", "", "", nil, "started on work@x.com (claude) in app"},
		{"a resume", "alice", whyResumed, "", nil, "was resumed on work@x.com (claude) in app"},
		{"a move", "alice", whyMoved, "", func(ts *termSession) { ts.label = "home@x.com" }, "moved to home@x.com (claude)"},
		{"words of its own", "alice", whyMoved, "moved here from work@x.com", nil, "moved here from work@x.com"},
		{"a shell", "", "", "", func(ts *termSession) {
			ts.kind, ts.account, ts.label, ts.provider = termKindShell, 0, "", ""
		}, "started a shell in app"},
		{"alice's shell", "alice", "", "", func(ts *termSession) {
			ts.kind, ts.account, ts.label, ts.provider = termKindShell, 0, "", ""
		}, "started by alice as a shell in app"},
		{"a shared terminal", "", "", "", func(ts *termSession) { ts.kind = termKindShared },
			"is shared from a terminal on this machine"},
		{"no folder", "alice", "", "", func(ts *termSession) { ts.cwd, ts.name = "", "notes" },
			"started by alice on work@x.com (claude)"},
	} {
		var tune []func(*termSession)
		if c.tune != nil {
			tune = append(tune, c.tune)
		}
		ts := sessIn(h.api, time.Now(), tune...)
		h.api.sessionStarted(ts, c.by, c.why, c.text)
		got := sessOf(h.api, events.SessionStarted, ts.id)
		if len(got) != 1 {
			t.Fatalf("%s: published %+v", c.what, got)
		}
		e := got[0]
		if e.Text != c.want || e.Reason != c.why || e.Account != ts.account || e.Name != ts.eventName() {
			t.Errorf("%s: %+v, want %q with reason %q", c.what, e, c.want, c.why)
		}
	}
}

/* --------------------------------------------------------------- ended --- */

// session.ended says why it ended and after how long, with the CLI's exit
// code: on its own, stopped by somebody, stopped for being left alone,
// interrupted when its holder went, or moved, in the words it was given.
func TestEndedSaysWhyAndHowLong(t *testing.T) {
	h := eventsServer(t, func(o *Options) { o.Terminal.IdleTimeout = 12 * time.Hour })
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		what   string
		end    func(*termSession)
		after  time.Duration
		code   int
		reason string
		text   string
		exit   int
	}{
		{"an exit", nil, time.Hour + 3*time.Minute + 20*time.Second, 0, whyExited, "ended with exit 0 after 1h 3m", 0},
		{"a stop", func(ts *termSession) { ts.kill("alice") }, 12*time.Minute + 5*time.Second, 129, whyStopped,
			"was stopped by alice after 12m (exit 129)", 129},
		{"a stop from this machine", func(ts *termSession) { ts.kill("local") }, 12 * time.Minute, 129, whyStopped,
			"was stopped after 12m (exit 129)", 129},
		{"a stop for being left alone", func(ts *termSession) { ts.kill("idle") }, 30 * time.Hour, 129, whyStopped,
			"was stopped after 12h with nobody attached", 129},
		{"a move", func(ts *termSession) { ts.endingAs(whyMoved, "is moving to home@x.com") }, 5 * time.Minute, 0, whyMoved,
			"is moving to home@x.com", 0},
		{"a move that is also a stop", func(ts *termSession) {
			ts.endingAs(whyMoved, "is moving to home@x.com")
			ts.kill("alice")
		}, 5 * time.Minute, 129, whyMoved, "is moving to home@x.com", 129},
		{"an interruption", func(ts *termSession) { ts.back = &heldTerm{state: heldInterrupted} }, 2 * time.Hour, -1,
			whyInterrupted, "was interrupted after 2h: its holder went away", -1},
		// Set rather than killed: a held far end built in memory has no
		// holder to signal.
		{"an interruption while stopping", func(ts *termSession) {
			ts.back, ts.stoppedBy = &heldTerm{state: heldInterrupted}, "alice"
		}, 2*time.Hour + 59*time.Second, -1, whyInterrupted, "was interrupted after 2h: its holder went away", -1},
		{"seconds", nil, 45 * time.Second, 3, whyExited, "ended with exit 3 after 45s", 3},
		{"days", nil, 52*time.Hour + 10*time.Minute, 0, whyExited, "ended with exit 0 after 2d 4h", 0},
		{"whole days", nil, 48 * time.Hour, 0, whyExited, "ended with exit 0 after 2d", 0},
	} {
		h.api.clock.set(func() time.Time { return at })
		ts := sessIn(h.api, at)
		if c.end != nil {
			c.end(ts)
		}
		h.api.clock.set(func() time.Time { return at.Add(c.after) })
		ts.finish(c.code)
		got := sessOf(h.api, events.SessionEnded, ts.id)
		if len(got) != 1 {
			t.Fatalf("%s: published %+v", c.what, got)
		}
		e := got[0]
		if e.Text != c.text || e.Reason != c.reason || e.Exit == nil || *e.Exit != c.exit ||
			e.Account != 1 || e.Name != "app" {
			t.Errorf("%s: %+v (exit %v), want %q with reason %q and exit %d", c.what, e, e.Exit, c.text, c.reason, c.exit)
		}
		ts.finish(c.code) // once is enough, and twice says nothing more
		if got := sessOf(h.api, events.SessionEnded, ts.id); len(got) != 1 {
			t.Errorf("%s: a second finish published %+v", c.what, got)
		}
	}
}

/* ------------------------------------------------------------- viewers --- */

// A viewer is a person, not a connection: alice on two devices joins once
// and leaves once, a connection that drops while another is open says
// nothing, and one that comes back within the settle says nothing either
// (Review Focus 4). A watcher is said to watch.
func TestViewersArePeopleNotConnections(t *testing.T) {
	setFor(t, &viewerSettle, 50*time.Millisecond)
	h := eventsServer(t)
	ts := sessIn(h.api, time.Now())
	first, second := sessConn("alice", "user", RoleControl, false), sessConn("alice", "user", RoleControl, false)
	ts.attach(first, 0, true)
	ts.attach(second, 0, true)
	joined := sessOf(h.api, events.ViewerJoined, ts.id)
	if len(joined) != 1 {
		t.Fatalf("alice on two connections joined %+v", joined)
	}
	if e := joined[0]; e.Viewer != "alice" || e.Reason != "user" || e.Text != "has alice attached, to control" ||
		e.Name != "app" || e.Account != 1 {
		t.Fatalf("alice joined as %+v", e)
	}

	ts.detach(first)
	sessQuiet(t, h.api, ts.id, 3*viewerSettle, events.ViewerLeft)
	ts.detach(second)
	if got := sessOf(h.api, events.ViewerLeft, ts.id); len(got) != 0 {
		t.Fatalf("alice was said to leave before the settle: %+v", got)
	}
	for end := time.Now().Add(5 * time.Second); len(sessOf(h.api, events.ViewerLeft, ts.id)) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("alice's leaving was never said")
		}
	}
	if e := sessOf(h.api, events.ViewerLeft, ts.id)[0]; e.Viewer != "alice" || e.Reason != "user" ||
		e.Text != "no longer has alice attached" || e.Name != "app" {
		t.Fatalf("alice left as %+v", e)
	}

	// Back after she was said to leave, she joins again. Gone and back
	// within the settle, nothing is said: the settle is long here, so the
	// test is not about how fast it gets from one line to the next.
	again := sessConn("alice", "user", RoleControl, false)
	ts.attach(again, 0, true)
	if got := sessOf(h.api, events.ViewerJoined, ts.id); len(got) != 2 {
		t.Fatalf("alice, back after leaving, joined %+v", got)
	}
	viewerSettle = time.Minute
	ts.detach(again)
	ts.attach(sessConn("alice", "user", RoleControl, false), 0, true)
	viewerSettle = 50 * time.Millisecond
	if got := sessOf(h.api, events.ViewerJoined, ts.id); len(got) != 2 {
		t.Fatalf("alice, back within the settle, joined %+v", got)
	}
	time.Sleep(3 * viewerSettle)
	if got := sessOf(h.api, events.ViewerLeft, ts.id); len(got) != 1 {
		t.Fatalf("alice, back within the settle, left %+v", got)
	}

	ts.attach(sessConn("bob", "token", RoleWatch, true), 0, true)
	joined = sessOf(h.api, events.ViewerJoined, ts.id)
	if e := joined[len(joined)-1]; e.Viewer != "bob" || e.Reason != "token" || e.Text != "has bob attached, to watch" {
		t.Fatalf("bob joined as %+v", e)
	}
	// One that may control but attached to watch watches.
	ts.attach(sessConn("carol", "user", RoleControl, true), 0, true)
	joined = sessOf(h.api, events.ViewerJoined, ts.id)
	if e := joined[len(joined)-1]; e.Viewer != "carol" || e.Text != "has carol attached, to watch" {
		t.Fatalf("carol, attached to watch, joined as %+v", e)
	}
	// The same name by another door is another person: a token called
	// alice is not alice signed in on the page.
	ts.attach(sessConn("alice", "token", RoleControl, false), 0, true)
	joined = sessOf(h.api, events.ViewerJoined, ts.id)
	if e := joined[len(joined)-1]; e.Viewer != "alice" || e.Reason != "token" {
		t.Fatalf("the token called alice joined as %+v", e)
	}
}

// This machine's own user, on the control socket, is never announced.
func TestLocalIsNeverAnnounced(t *testing.T) {
	setFor(t, &viewerSettle, 20*time.Millisecond)
	h := eventsServer(t)
	ts := sessIn(h.api, time.Now())
	tc := sessConn("local", "local", RoleControl, false)
	ts.attach(tc, 0, true)
	ts.detach(tc)
	sessQuiet(t, h.api, ts.id, 10*viewerSettle, events.ViewerJoined, events.ViewerLeft)
}

// Nothing is said of a viewer leaving a session that has ended, nor of one
// attaching to it: it stays listed for a while, and replays.
func TestNoViewerLeavesAnEndedSession(t *testing.T) {
	setFor(t, &viewerSettle, 20*time.Millisecond)
	h := eventsServer(t)
	ts := sessIn(h.api, time.Now())
	alice := sessConn("alice", "user", RoleControl, false)
	ts.attach(alice, 0, true)
	if got := sessOf(h.api, events.ViewerJoined, ts.id); len(got) != 1 {
		t.Fatalf("alice joined %+v", got)
	}
	ts.finish(0)
	ts.detach(alice)
	late := sessConn("bob", "user", RoleControl, false)
	ts.attach(late, 0, true)
	ts.detach(late)
	sessQuiet(t, h.api, ts.id, 10*viewerSettle, events.ViewerLeft)
	if got := sessOf(h.api, events.ViewerJoined, ts.id); len(got) != 1 {
		t.Fatalf("an ended session was joined: %+v", got)
	}

	// And a viewer who left just before the end, within the settle, is
	// not said to have left either.
	ts = sessIn(h.api, time.Now())
	alice = sessConn("alice", "user", RoleControl, false)
	ts.attach(alice, 0, true)
	viewerSettle = time.Minute
	ts.detach(alice)
	ts.finish(0)
	viewerSettle = 20 * time.Millisecond
	sessQuiet(t, h.api, ts.id, 10*viewerSettle, events.ViewerLeft)
}

/* --------------------------------------------------------------- tokens --- */

// A session's token ending is said once, as account.dead with reason
// token: named by the account, and saying which session can no longer
// reach it. A claude session on its stored login says its login ended.
// The sweep says it even where the idle timeout is off.
func TestATokenThatEndsIsSaidOnce(t *testing.T) {
	until := time.Now().Add(-time.Minute)
	for _, c := range []struct {
		what     string
		provider string
		onToken  bool
		idle     time.Duration
		words    string
	}{
		{"a codex token", "codex", false, 0, "the token it was started with ended at "},
		{"a claude token", "claude", true, 0, "the token it was started with ended at "},
		{"a claude login", "claude", false, 0, "its login ended at "},
		{"no idle timeout", "codex", false, -1, "the token it was started with ended at "},
	} {
		h := eventsServer(t, func(o *Options) { o.Terminal.IdleTimeout = c.idle })
		ts := sessIn(h.api, time.Now().Add(-time.Hour), func(ts *termSession) {
			ts.provider, ts.onToken, ts.tokenUntil, ts.name = c.provider, c.onToken, until, "refactor"
		})
		h.api.addTerminal(ts)
		t.Cleanup(func() { h.api.dropTerminal(ts.id) })
		h.api.sweepTerminals()
		h.api.sweepTerminals()
		got := sessOf(h.api, events.AccountDead, ts.id)
		if len(got) != 1 {
			t.Fatalf("%s: two sweeps said %+v", c.what, got)
		}
		want := "can no longer be reached from refactor: " + c.words + sessUTC(until)
		if e := got[0]; e.Reason != "token" || e.Account != 1 || e.Name != "work@x.com" || e.Text != want {
			t.Errorf("%s: %+v, want %q", c.what, e, want)
		}
	}

	// One not yet over, or one that has ended, says nothing.
	h := eventsServer(t)
	later := sessIn(h.api, time.Now(), func(ts *termSession) { ts.tokenUntil = time.Now().Add(time.Hour) })
	over := sessIn(h.api, time.Now(), func(ts *termSession) { ts.tokenUntil = until })
	over.finish(0)
	for _, ts := range []*termSession{later, over} {
		h.api.addTerminal(ts)
		t.Cleanup(func() { h.api.dropTerminal(ts.id) })
	}
	h.api.sweepTerminals()
	if got := published(h.api, events.AccountDead); len(got) != 0 {
		t.Fatalf("a token still good and a session that ended said %+v", got)
	}
}

// A session is on a token, rather than on its account's stored login, when
// the time a holder says its credential lasts is not the login's end.
func TestASessionKnowsWhatItRunsOn(t *testing.T) {
	end := time.Date(2026, 12, 1, 21, 20, 0, 0, time.UTC)
	login := &rota.Account{Provider: "claude", Extra: map[string]string{
		"refresh_token_expires_at": strconv.FormatInt(end.UnixMilli(), 10)}}
	for _, c := range []struct {
		what  string
		a     *rota.Account
		until time.Time
		want  bool
	}{
		{"the login's end", login, end, false},
		{"another end", login, end.Add(-20 * time.Hour), true},
		{"no end", login, time.Time{}, false},
		{"codex", &rota.Account{Provider: "codex"}, end, false},
		{"no account", nil, end, false},
	} {
		if got := onTokenFor(c.a, c.until); got != c.want {
			t.Errorf("%s: on a token is %v, want %v", c.what, got, c.want)
		}
	}
}
