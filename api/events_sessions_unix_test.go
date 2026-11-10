//go:build linux || darwin

package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/share"
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

// sessFakesOnly runs setup, and then leaves on PATH only what setup put in
// front of it, the directories of its fakes: no CLI of this machine's can be
// found while the test runs.
func sessFakesOnly[T any](t *testing.T, setup func() T) T {
	t.Helper()
	before := os.Getenv("PATH")
	v := setup()
	added, ok := strings.CutSuffix(os.Getenv("PATH"), string(os.PathListSeparator)+before)
	if !ok || added == "" {
		t.Fatalf("setup put nothing in front of PATH: %q", os.Getenv("PATH"))
	}
	t.Setenv("PATH", added)
	return v
}

// A terminal this server runs says it started, by whom, on which account
// and in which folder; that it ended on its own when its CLI exits; and
// that it was stopped, by whom, when it is deleted.
func TestStartedAndEndedOnALocalTerminal(t *testing.T) {
	p := sessFakesOnly(t, func() *terms { return withTerminals(t) })
	doc := p.sessStart(p.alice, nil)
	id := doc["id"].(string)
	e := sessWait(t, p.api, events.SessionStarted, id, 5*time.Second)
	if want := "started by alice on " + sessWords(doc); e.Text != want || e.Reason != "" ||
		e.Account != 1 || e.Name != filepath.Base(doc["cwd"].(string)) {
		t.Fatalf("the start said %+v, want %q", e, want)
	}
	// Account 1 holds no refresh token, so its claude is handed the token in
	// its environment, and its token's end is said as a token's.
	if !p.api.findTerminal(id).onToken {
		t.Error("a claude terminal on CLAUDE_CODE_OAUTH_TOKEN is not known to run on a token")
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
	p := sessFakesOnly(t, func() *terms { return withHeld(t, nil) })
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
	p := sessFakesOnly(t, func() *terms { return withHeld(t, nil) })
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
	p := sessFakesOnly(t, func() *terms { return withTerminals(t) })
	ts, err := p.api.startTerminal(sessAs(), &termRequest{Account: 1, Cols: 80, Rows: 24, why: whyResumed})
	if err != nil {
		t.Fatal(err)
	}
	e := sessWait(t, p.api, events.SessionStarted, ts.id, 5*time.Second)
	if want := "was resumed on " + sessWords(ts.view()); e.Reason != whyResumed || e.Text != want {
		t.Fatalf("a resume said %+v, want %q", e, want)
	}

	q := sessFakesOnly(t, func() *terms { return withHeld(t, nil) })
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

// A terminal shared from this machine says it is shared, naming nobody, and
// says it ended with the code its sharer sends.
func TestASharedTerminalSaysItStartedAndEnded(t *testing.T) {
	p := sessFakesOnly(t, func() *terms { return withTerminals(t) })
	f, _ := p.offer(share.HelloMsg{Account: 1, AccountLabel: "work@x.com", Provider: "claude", Cwd: "/x/app"})
	e := sessWait(t, p.api, events.SessionStarted, f.id, 5*time.Second)
	if e.Text != "is shared from a terminal on this machine" || e.Reason != "" || e.Name != "app" || e.Account != 1 {
		t.Fatalf("a share said %+v", e)
	}
	if err := f.c.SendJSON(share.Exit, share.ExitMsg{Code: 7}); err != nil {
		t.Fatal(err)
	}
	e = sessWait(t, p.api, events.SessionEnded, f.id, 10*time.Second)
	if e.Reason != whyExited || e.Exit == nil || *e.Exit != 7 || !strings.HasPrefix(e.Text, "ended with exit 7 after ") {
		t.Fatalf("a shared terminal's exit said %+v", e)
	}
	if n := len(sessOf(p.api, events.SessionStarted, f.id)); n != 1 {
		t.Fatalf("a share was said to start %d times", n)
	}
}

// A terminal startTerminal started but could not list, as Stop begins, is
// ended without a word: it was never said to have started.
func TestATerminalNeverListedSaysNothing(t *testing.T) {
	var out logBuf
	p := sessFakesOnly(t, func() *terms {
		return withTerminals(t, func(o *Options) {
			o.Log = slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo}))
		})
	})
	p.api.termsMu.Lock()
	p.api.stopping = true
	p.api.termsMu.Unlock()
	if _, err := p.api.startTerminal(sessAs(), &termRequest{Account: 1, Cols: 80, Rows: 24}); err == nil {
		t.Fatal("a terminal was listed on a server that is stopping")
	}
	for end := time.Now().Add(10 * time.Second); !strings.Contains(out.String(), `msg="terminal ended"`); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the terminal that was not listed never ended:\n%s", out.String())
		}
	}
	if got := published(p.api, events.SessionStarted, events.SessionEnded); len(got) != 0 {
		t.Fatalf("a terminal never listed said %+v", got)
	}
}

// A person whose page was attached to the daemon before is not said to join
// again when it reconnects to the next one, within twice the settle of that
// daemon taking the session up; a person who attaches after that is.
func TestARestartSaysNoViewerAgain(t *testing.T) {
	p := sessFakesOnly(t, func() *terms { return withHeld(t, nil) })
	id := p.start(nil)["id"].(string)
	p.attach(id, p.alice, "").waitType("hello")
	if e := sessWait(t, p.api, events.ViewerJoined, id, 5*time.Second); e.Viewer != "alice" {
		t.Fatalf("alice joined as %+v", e)
	}
	q := p.restart()
	forward := q.frozenClock()
	q.attach(id, q.alice, "").waitType("hello")
	sessQuiet(t, q.api, id, 200*time.Millisecond, events.ViewerJoined)
	forward(2 * viewerSettle)
	q.attach(id, q.bob, "").waitType("hello")
	if e := sessWait(t, q.api, events.ViewerJoined, id, 5*time.Second); e.Viewer != "bob" {
		t.Fatalf("after the window, %+v joined", e)
	}
	if got := sessOf(q.api, events.ViewerJoined, id); len(got) != 1 {
		t.Fatalf("the next daemon said %+v", got)
	}
}

// sessOnEnds makes the harness's store three claude accounts: 1 with no
// stored login, so its sessions run on a token; 3 whose stored login ends at
// until; and 5 on a long-lived token that ends then. It starts a held
// session on each, whose holder (FAKE_HOLDER_TOKEN_UNTIL) says that what it
// runs on lasts until then, and returns their ids by account.
func sessOnEnds(t *testing.T, p *terms, until time.Time) map[int]string {
	t.Helper()
	blob := fmt.Sprintf(`{"accounts":[
	 {"id":1,"provider":"claude","email":"a@x","token":{"accessToken":"tok-1"}},
	 {"id":3,"provider":"claude","email":"b@x","token":{"accessToken":"tok-3"},"extra":{"refresh_token_expires_at":"%d"}},
	 {"id":5,"provider":"claude","email":"long@x","token":{"accessToken":"tok-5"},"long":{"accessToken":"long-token-5555","expiresAt":%d}}]}`,
		until.UnixMilli(), until.UnixMilli())
	if err := os.WriteFile(filepath.Join(p.dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	ids := map[int]string{}
	for _, a := range []int{1, 3, 5} {
		doc := p.sessStart(p.alice, map[string]any{"account": a, "cols": 80, "rows": 24, "label": fmt.Sprintf("s%d", a)})
		ids[a] = doc["id"].(string)
		sessWait(t, p.api, events.SessionStarted, ids[a], 5*time.Second)
	}
	return ids
}

// sessTokensEnd moves s's frozen clock to a minute before until, where
// nothing is said of any session's token, and then past it, where two
// sweeps say each one's end once, in the words of what it runs on.
func sessTokensEnd(t *testing.T, s *Server, forward func(time.Duration), until time.Time, ids map[int]string) {
	t.Helper()
	forward(until.Sub(s.now()) - time.Minute)
	s.sweepTerminals()
	for _, id := range ids {
		if got := sessOf(s, events.AccountDead, id); len(got) != 0 {
			t.Fatalf("a minute before the end, %s said %+v", id, got)
		}
	}
	forward(2 * time.Minute)
	s.sweepTerminals()
	s.sweepTerminals()
	for a, c := range map[int]struct{ name, words string }{
		1: {"a@x", "the token it was started with ended at "},
		3: {"b@x", "its login ended at "},
		5: {"long@x", "the token it was started with ended at "},
	} {
		got := sessOf(s, events.AccountDead, ids[a])
		want := fmt.Sprintf("can no longer be reached from s%d: %s%s", a, c.words, sessUTC(until))
		if len(got) != 1 || got[0].Reason != "token" || got[0].Account != a || got[0].Name != c.name || got[0].Text != want {
			t.Errorf("account %d's session said %+v, want once %q", a, got, want)
		}
	}
	if ts := s.findTerminal(ids[5]); ts == nil || !ts.longToken {
		t.Error("the session on a long-lived token is not known to be on one")
	}
}

// A held session's token, or its login, ending is said once, in the words
// of what it runs on, from what its holder said when it started.
func TestAHeldSessionsTokenEndIsSaidOnce(t *testing.T) {
	until := time.Now().Add(72 * time.Hour).Truncate(time.Millisecond)
	p := sessFakesOnly(t, func() *terms {
		return withHeld(t, []string{"FAKE_HOLDER_TOKEN_UNTIL=" + until.Format(time.RFC3339Nano)})
	})
	ids := sessOnEnds(t, p, until)
	sessTokensEnd(t, p.api, p.frozenClock(), until, ids)
}

// So is an adopted session's, from the record its holder wrote and from its
// account as the daemon that took it up read it; the daemon that let it go
// said nothing of it.
func TestAnAdoptedSessionsTokenEndIsSaidOnce(t *testing.T) {
	until := time.Now().Add(72 * time.Hour).Truncate(time.Millisecond)
	p := sessFakesOnly(t, func() *terms {
		return withHeld(t, []string{"FAKE_HOLDER_TOKEN_UNTIL=" + until.Format(time.RFC3339Nano)})
	})
	ids := sessOnEnds(t, p, until)
	q := p.restart()
	for _, id := range ids {
		if doc := q.describe(id); doc["state"] != heldLive {
			t.Fatalf("the next daemon did not take %s up: %v", id, doc)
		}
	}
	sessTokensEnd(t, q.api, q.frozenClock(), until, ids)
	if got := published(p.api, events.AccountDead); len(got) != 0 {
		t.Fatalf("the daemon that let them go said %+v", got)
	}
}
