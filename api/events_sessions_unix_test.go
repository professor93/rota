//go:build linux || darwin

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
)

// What a session says about itself when a CLI on a pseudo-terminal, or a
// holder, runs it: the fake CLI from internal/fakecli and the fake holder
// (fakeholder_test.go), never a real one.

// sessWait waits up to within for s to have said something of type typ
// about session id, and returns the first.
func sessWait(t *testing.T, s *Server, typ, id string, within time.Duration) events.Event {
	t.Helper()
	for end := time.Now().Add(within); ; time.Sleep(5 * time.Millisecond) {
		if got := sessOf(s, typ, id); len(got) > 0 {
			return got[0]
		}
		if time.Now().After(end) {
			t.Fatalf("no %s about %s within %v; what was said: %+v", typ, id, within, published(s))
		}
	}
}

// sessStart starts a terminal as whoever holds token, and returns its
// description.
func (p *terms) sessStart(token string, body map[string]any) map[string]any {
	p.tt.Helper()
	if body == nil {
		body = map[string]any{"account": 1, "cols": 80, "rows": 24}
	}
	resp, raw := p.as(token, "POST", "/v1/terminals", body)
	if resp.StatusCode != http.StatusCreated {
		p.tt.Fatalf("starting a terminal: %d %s", resp.StatusCode, raw)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		p.tt.Fatal(err)
	}
	return doc
}

// sessWords is the account and folder a session's start names, from its
// description.
func sessWords(doc map[string]any) string {
	a, _ := doc["account"].(map[string]any)
	return fmt.Sprintf("%v (%v) in %s", a["label"], a["provider"], filepath.Base(doc["cwd"].(string)))
}

// sessAs is a request as alice signed in on the page, for a test that
// starts a terminal without HTTP.
func sessAs() *http.Request {
	return withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/terminals", nil),
		&Principal{Name: "alice", Role: RoleControl, Via: "user"})
}

// A terminal this server runs says it started, by whom, on which account
// and in which folder; that it ended on its own when its CLI exits; and
// that it was stopped, by whom, when it is deleted.
func TestStartedAndEndedOnALocalTerminal(t *testing.T) {
	p := withTerminals(t)
	doc := p.sessStart(p.alice, nil)
	id := doc["id"].(string)
	e := sessWait(t, p.api, events.SessionStarted, id, 5*time.Second)
	if want := "started by alice on " + sessWords(doc); e.Text != want || e.Reason != "" ||
		e.Account != 1 || e.Name != filepath.Base(doc["cwd"].(string)) {
		t.Fatalf("the start said %+v, want %q", e, want)
	}

	tm := p.attach(id, p.alice, "")
	tm.waitOut("ready ")
	tm.typeIn("bye\n")
	e = sessWait(t, p.api, events.SessionEnded, id, 10*time.Second)
	if e.Reason != whyExited || e.Exit == nil || *e.Exit != 0 || !strings.HasPrefix(e.Text, "ended with exit 0 after ") {
		t.Fatalf("an exit said %+v", e)
	}

	other := p.sessStart(p.alice, nil)["id"].(string)
	sessWait(t, p.api, events.SessionStarted, other, 5*time.Second)
	if resp, raw := p.as(p.alice, "DELETE", "/v1/terminals/"+other, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, raw)
	}
	e = sessWait(t, p.api, events.SessionEnded, other, 10*time.Second)
	exit := p.waitEnded(other)["exit"]
	if e.Reason != whyStopped || e.Exit == nil || float64(*e.Exit) != exit ||
		!strings.HasPrefix(e.Text, "was stopped by alice after ") ||
		!strings.HasSuffix(e.Text, fmt.Sprintf("s (exit %d)", *e.Exit)) {
		t.Fatalf("a delete said %+v, and the terminal says exit %v", e, exit)
	}
	for _, id := range []string{id, other} {
		if n := len(sessOf(p.api, events.SessionStarted, id)) + len(sessOf(p.api, events.SessionEnded, id)); n != 2 {
			t.Errorf("%s was said to start and end %d times in all", id, n)
		}
	}
}

// A held session says it started, and that it ended with its CLI's exit
// code; one whose holder is killed from outside says it was interrupted.
func TestHeldSessionsStartEndAndAreInterrupted(t *testing.T) {
	p := withHeld(t, nil)
	doc := p.sessStart(p.alice, nil)
	id := doc["id"].(string)
	if e := sessWait(t, p.api, events.SessionStarted, id, 5*time.Second); e.Text != "started by alice on "+sessWords(doc) {
		t.Fatalf("a held start said %+v", e)
	}
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("exit 4\r")
	e := sessWait(t, p.api, events.SessionEnded, id, 10*time.Second)
	if e.Reason != whyExited || e.Exit == nil || *e.Exit != 4 || !strings.HasPrefix(e.Text, "ended with exit 4 after ") {
		t.Fatalf("a held exit said %+v", e)
	}

	doc = p.sessStart(p.alice, nil)
	killed, pid := doc["id"].(string), int(doc["pid"].(float64))
	sessWait(t, p.api, events.SessionStarted, killed, 5*time.Second)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	e = sessWait(t, p.api, events.SessionEnded, killed, 10*time.Second)
	if e.Reason != whyInterrupted || e.Exit == nil || *e.Exit != -1 ||
		!strings.HasPrefix(e.Text, "was interrupted after ") || !strings.HasSuffix(e.Text, ": its holder went away") {
		t.Fatalf("a killed holder said %+v", e)
	}
}

// A daemon that restarts takes its sessions up without a word: the one that
// stopped let them go, which ends nothing, and the next one says nothing of
// a session it adopted until that session ends.
func TestARestartAnnouncesNoSession(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	sessWait(t, p.api, events.SessionStarted, id, 5*time.Second)
	q := p.restart()
	if got := sessOf(p.api, events.SessionEnded, id); len(got) != 0 {
		t.Fatalf("the daemon that let %s go said %+v", id, got)
	}
	if doc := q.describe(id); doc["state"] != heldLive {
		t.Fatalf("the next daemon did not take %s up: %v", id, doc)
	}
	sessQuiet(t, q.api, id, 300*time.Millisecond, events.SessionStarted, events.SessionEnded)

	tm := q.attach(id, q.alice, "")
	tm.waitType("hello")
	tm.typeIn("exit 0\r")
	e := sessWait(t, q.api, events.SessionEnded, id, 10*time.Second)
	if e.Reason != whyExited || e.Exit == nil || *e.Exit != 0 {
		t.Fatalf("the adopted session ended with %+v", e)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(sessOf(q.api, events.SessionEnded, id)); n != 1 {
		t.Fatalf("the adopted session was said to end %d times", n)
	}
	if got := sessOf(q.api, events.SessionStarted, id); len(got) != 0 {
		t.Fatalf("the adopted session was said to start: %+v", got)
	}
}

// Why a session started reaches its session.started, on a terminal this
// server runs and on a held one: a path that resumes or moves one says so.
func TestWhyReachesTheStart(t *testing.T) {
	p := withTerminals(t)
	ts, err := p.api.startTerminal(sessAs(), &termRequest{Account: 1, Cols: 80, Rows: 24, why: whyResumed})
	if err != nil {
		t.Fatal(err)
	}
	e := sessWait(t, p.api, events.SessionStarted, ts.id, 5*time.Second)
	if want := "was resumed on " + sessWords(ts.view()); e.Reason != whyResumed || e.Text != want {
		t.Fatalf("a resume said %+v, want %q", e, want)
	}

	q := withHeld(t, nil)
	ts, err = q.api.startTerminal(sessAs(), &termRequest{Account: 1, Cols: 80, Rows: 24, why: whyMoved})
	if err != nil {
		t.Fatal(err)
	}
	e = sessWait(t, q.api, events.SessionStarted, ts.id, 5*time.Second)
	a, _ := ts.view()["account"].(map[string]any)
	if want := fmt.Sprintf("moved to %v (%v)", a["label"], a["provider"]); e.Reason != whyMoved || e.Text != want {
		t.Fatalf("a held move said %+v, want %q", e, want)
	}
}
