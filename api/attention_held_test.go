//go:build linux || darwin

package api

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
)

// A held session's attention reaching the server's events, with the fake
// holder saying what a CLI's own signals would (fakeholder_test.go's typed
// title, status and bell lines). The daemon's own tick looks at the session
// every second as well, so these wait for what is said rather than assume
// whose look said it, and look at the same clock the tick does.

// attnWithHeld is withHeld with nothing on PATH but the fakes the harness
// put ahead of it: no test here runs a CLI, and none may find a real one.
func attnWithHeld(t *testing.T, env []string) *terms {
	t.Helper()
	before := os.Getenv("PATH")
	p := withHeld(t, env)
	fakes := strings.TrimSuffix(os.Getenv("PATH"), string(os.PathListSeparator)+before)
	if fakes == "" || fakes == os.Getenv("PATH") {
		t.Fatalf("the harness put no fakes ahead of PATH: %q", os.Getenv("PATH"))
	}
	t.Setenv("PATH", fakes)
	return p
}

// attnHeld is the held session id on s.
func attnHeld(t *testing.T, s *Server, id string) *heldTerm {
	t.Helper()
	for _, ts := range s.everyTerminal() {
		if h, ok := ts.back.(*heldTerm); ok && ts.id == id {
			return h
		}
	}
	t.Fatalf("%s is not a held session on this server", id)
	return nil
}

// attnUntil waits for h to be as ok says, read under its lock.
func attnUntil(t *testing.T, h *heldTerm, what string, ok func(*heldTerm) bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		h.mu.Lock()
		done := ok(h)
		h.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("the session never came to %s", what)
		}
	}
}

// attnSaid is every session.attention s has published for id, in order.
func attnSaid(s *Server, id string) []events.Event {
	var out []events.Event
	for _, e := range published(s, events.SessionAttention) {
		if e.Session == id {
			out = append(out, e)
		}
	}
	return out
}

// attnReasons is what each of evs was said for.
func attnReasons(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Reason
	}
	return out
}

// attnAttend looks at s's held sessions at now until what s has said for
// id is want, failing the test if it says more, or does not within a few
// seconds. It looks itself rather than wait for the tick, which may have
// looked first and be about to publish.
func attnAttend(t *testing.T, s *Server, id string, now func() time.Time, want ...string) []events.Event {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for {
		s.attendAll(now())
		got := attnSaid(s, id)
		if len(got) > len(want) || !slices.Equal(attnReasons(got), want[:len(got)]) {
			t.Fatalf("the session said %v, want %v", attnReasons(got), want)
		}
		if len(got) == len(want) {
			return got
		}
		if time.Now().After(end) {
			t.Fatalf("the session said %v, want %v", attnReasons(got), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// attnType types line into the session through tm, waits for what it
// reports to reach the server, and moves the frozen clock on past the
// typing, looking at the session on the way as the daemon's tick does: at
// the moment of typing and a second before it is attnTyping ago, while the
// person typing is still there and nothing may be said.
func attnType(t *testing.T, p *terms, tm *term, h *heldTerm, forward func(time.Duration), line string, ok func(*heldTerm) bool) {
	t.Helper()
	id := h.ts.id
	before := attnReasons(attnSaid(p.api, id))
	tm.typeIn(line + "\r")
	attnUntil(t, h, "what "+line+" reports", ok)
	h.mu.Lock()
	typed := h.typedAt
	h.mu.Unlock()
	if !typed.Equal(p.api.now()) {
		t.Fatalf("typing %q left typedAt at %v, want %v", line, typed, p.api.now())
	}
	p.api.attendAll(p.api.now())
	if got := attnReasons(attnSaid(p.api, id)); !slices.Equal(got, before) {
		t.Fatalf("as %q was typed the session said %v, after %v", line, got, before)
	}
	forward(attnTyping - time.Second)
	p.api.attendAll(p.api.now())
	if got := attnReasons(attnSaid(p.api, id)); !slices.Equal(got, before) {
		t.Fatalf("%v after %q was typed the session said %v, after %v", attnTyping-time.Second, line, got, before)
	}
	forward(time.Second)
}

// A held session whose registry says it waits on its person is said to,
// once for each thing it waits for, under the session's own name and never
// its conversation's, and only once the typing that led to it is
// attnTyping ago.
func TestAttentionReachesTheBus(t *testing.T) {
	p := attnWithHeld(t, []string{"FAKE_HOLDER_CONVERSATION_NAME=secret plans"})
	work := filepath.Join(p.root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	id := p.start(map[string]any{"account": 1, "cwd": work})["id"].(string)
	h := attnHeld(t, p.api, id)
	attnUntil(t, h, "its first state", func(h *heldTerm) bool { return h.convName == "secret plans" })
	forward := p.frozenClock()
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")

	attnType(t, p, tm, h, forward, "status working", func(h *heldTerm) bool { return h.reg.Status == "working" })
	attnAttend(t, p.api, id, p.api.now)
	attnType(t, p, tm, h, forward, "status idle waiting", func(h *heldTerm) bool { return h.reg.WaitingFor == "waiting" })
	attnAttend(t, p.api, id, p.api.now, "question")
	attnType(t, p, tm, h, forward, "status idle permission", func(h *heldTerm) bool { return h.reg.WaitingFor == "permission" })
	got := attnAttend(t, p.api, id, p.api.now, "question", "permission")
	forward(time.Minute)
	attnAttend(t, p.api, id, p.api.now, "question", "permission")

	for i, text := range []string{"is asking you something", "is waiting for your permission"} {
		e := got[i]
		if e.Name != "work" || e.Account != 1 || e.Text != text {
			t.Errorf("attention %d was published as %+v, want work, account 1, %q", i+1, e, text)
		}
		if strings.Contains(e.Name+e.Text, "secret") {
			t.Errorf("attention %d carries the conversation's name: %+v", i+1, e)
		}
	}
	if q := published(p.api, events.SessionQuiet); len(q) != 0 {
		t.Errorf("a session that printed nothing was said to have gone quiet: %v", q)
	}
}

// A bell and a title, with no registry to speak for the session, are said
// as their own reasons; and a bell rung again within attnBell is said once
// that window ends, not at once.
func TestABellAndATitleReachTheBus(t *testing.T) {
	p := attnWithHeld(t, []string{"FAKE_HOLDER_DROP_AT=1"})
	id := p.start(nil)["id"].(string)
	h := attnHeld(t, p.api, id)
	forward := p.frozenClock()
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")

	attnType(t, p, tm, h, forward, "bell", func(h *heldTerm) bool { return h.bells == 1 })
	attnAttend(t, p.api, id, p.api.now, "bell")
	// Said when the clock read attnTyping; the second ring comes attnTyping
	// after that, and is said attnBell after the first was.
	attnType(t, p, tm, h, forward, "bell", func(h *heldTerm) bool { return h.bells == 2 })
	attnAttend(t, p.api, id, p.api.now, "bell")
	forward(attnBell - attnTyping - time.Second)
	attnAttend(t, p.api, id, p.api.now, "bell")
	forward(time.Second)
	attnAttend(t, p.api, id, p.api.now, "bell", "bell")

	attnType(t, p, tm, h, forward, "title ⠂ Tidy", func(h *heldTerm) bool { return h.title == "⠂ Tidy" })
	attnAttend(t, p.api, id, p.api.now, "bell", "bell")
	attnType(t, p, tm, h, forward, "title ✳ Tidy", func(h *heldTerm) bool { return h.title == "✳ Tidy" })
	got := attnAttend(t, p.api, id, p.api.now, "bell", "bell", "title")
	if got[0].Text != "rang its bell" || got[2].Text != "is idle again" {
		t.Errorf("the bell and the title were said as %q and %q", got[0].Text, got[2].Text)
	}

	// A link that drops and comes back opens with the State the holder has
	// now, the bells and title above, not the one it started with.
	tm.typeIn("count 3\r")
	p.waitOffset(id, 3*len("line-0001\r\n"))
	h.mu.Lock()
	bells, title := h.bells, h.title
	h.mu.Unlock()
	if bells != 2 || title != "✳ Tidy" {
		t.Errorf("after the link came back the session has %d bells and title %q", bells, title)
	}
}

// A daemon that takes up a session already waiting for its person says
// nothing about it: the daemon before said it. Its first look waits for
// what the holder says, which here it says late, so the record's guess is
// never taken for the baseline. What changes after that is said (Review
// Focus 4).
func TestARestartSaysNothingAboutAWaitingSession(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p := attnWithHeld(t, []string{"FAKE_HOLDER_WAITING=permission", "FAKE_HOLDER_TITLE=✳ x",
		"FAKE_HOLDER_PRINT=hello", "FAKE_HOLDER_STATE_AFTER=" + gate})
	id := p.start(nil)["id"].(string)
	h := attnHeld(t, p.api, id)
	attnUntil(t, h, "waiting", func(h *heldTerm) bool { return h.reg.WaitingFor == "permission" })
	attnAttend(t, p.api, id, p.api.now, "permission")

	if err := os.Remove(gate); err != nil {
		t.Fatal(err)
	}
	q := p.restart()
	h = attnHeld(t, q.api, id)
	forward := q.frozenClock()
	for range 3 {
		q.api.attendAll(q.api.now())
		forward(time.Minute)
	}
	h.mu.Lock()
	adopted, replayTo, linked, stated, looked := h.adopted, h.replayTo, h.linkedAt, h.stated, h.attn.looked
	h.mu.Unlock()
	if !adopted || replayTo != int64(len("hello")) || linked.IsZero() {
		t.Errorf("the session was taken up as adopted %t, replaying to %d, linked at %v", adopted, replayTo, linked)
	}
	if stated || looked {
		t.Fatalf("before its holder said its state, the session was looked at (%t, stated %t)", looked, stated)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	attnUntil(t, h, "its first state", func(h *heldTerm) bool { return h.stated })
	for range 3 {
		q.api.attendAll(q.api.now())
		forward(time.Hour)
	}
	if got := attnSaid(q.api, id); len(got) != 0 {
		t.Fatalf("the daemon that took the session up said %v", attnReasons(got))
	}

	tm := q.attach(id, q.alice, "")
	tm.waitType("hello")
	attnType(t, q, tm, h, forward, "status idle", func(h *heldTerm) bool { return h.reg.WaitingFor == "" })
	attnAttend(t, q.api, id, q.api.now)
	attnType(t, q, tm, h, forward, "status idle permission", func(h *heldTerm) bool { return h.reg.WaitingFor == "permission" })
	attnAttend(t, q.api, id, q.api.now, "permission")
}
