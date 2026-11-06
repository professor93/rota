//go:build linux || darwin

package api

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// How a held terminal ends, and how starting one is refused, with the
// harness held_test.go keeps.

/* ------------------------------------------------------------- the ending --- */

// The CLI exits with two people watching, and both are told how (Review
// Focus 5). The session is then described as ended, with the code.
func TestTheExitReachesEveryViewer(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	one := p.attach(id, p.alice, "")
	one.waitType("hello")
	two := p.attach(id, p.bob, "mode=watch")
	two.waitType("hello")
	one.typeIn("exit 4\r")
	for _, tm := range []*term{one, two} {
		if doc := tm.waitType("exit"); doc["code"] != float64(4) {
			t.Fatalf("the exit frame is %v", doc)
		}
	}
	doc := p.waitEnded(id)
	if doc["exit"] != float64(4) || doc["state"] != "ended" || doc["status"] != "stopped" {
		t.Fatalf("an ended held session reads %v", doc)
	}
}

// Ending a held session asks its holder to hang the CLI up, over the link,
// and the holder records how it ended before it lets go — well before the
// kill that follows a holder that does not.
func TestDeletingAHeldSessionStopsItThroughItsHolder(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	asked := time.Now()
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, raw)
	}
	doc := p.waitEnded(id)
	if took := time.Since(asked); took >= termKillAfter {
		t.Fatalf("it took %s to end, which is the kill and not the hangup", took)
	}
	if doc["exit"] != float64(129) || doc["state"] != "ended" {
		t.Fatalf("a session its holder stopped reads %v", doc)
	}
	// The exit frame comes first and the lock goes after it, as the holder
	// records its ending in between.
	lock := holdwire.LockPath(holdwire.Dir(p.terminals(), id))
	for range 200 {
		if !holdwire.Locked(lock) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the holder still holds the session's lock")
}

// A holder that will not stop is killed, a little after the time a holder
// has to kill its own CLI — and a holder that vanished without recording an
// exit reads interrupted.
func TestAHolderThatWillNotStopIsKilled(t *testing.T) {
	was := termKillAfter
	termKillAfter = 100 * time.Millisecond
	t.Cleanup(func() { termKillAfter = was })
	p := withHeld(t, []string{"FAKE_HOLDER_DEAF=1"})
	doc := p.start(nil)
	id, pid := doc["id"].(string), int(doc["pid"].(float64))
	if resp, raw := p.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE: %d %s", resp.StatusCode, raw)
	}
	time.Sleep(termKillAfter + time.Second)
	if !alive(pid) {
		t.Fatal("the holder was killed before it had its time to stop its CLI")
	}
	doc = p.waitEnded(id)
	if !gone(pid) {
		t.Fatal("the holder outlived its kill")
	}
	if doc["state"] != "interrupted" || doc["exit"] != float64(-1) {
		t.Fatalf("a killed holder reads %v", doc)
	}
}

// A holder killed from outside in the middle of printing — the OOM killer,
// somebody's kill -9 — leaves its session interrupted, with no exit and the
// conversation it last named. Nothing else is touched: a session beside it
// goes on as before, and no holder is started to take its place.
func TestAHolderKilledFromOutsideMidOutputReadsInterrupted(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_CONVERSATION=conv-9"})
	doc := p.start(nil)
	id, pid := doc["id"].(string), int(doc["pid"].(float64))
	doc2 := p.start(map[string]any{"account": 3, "cols": 80, "rows": 24})
	id2, pid2 := doc2["id"].(string), int(doc2["pid"].(float64))
	for range 500 {
		if p.describe(id)["conversation"] == "conv-9" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("count 200\r")
	tm.waitOut("line-0020\r\n")
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	doc = p.waitEnded(id)
	if doc["state"] != "interrupted" || doc["exit"] != float64(-1) || doc["status"] != "stopped" {
		t.Fatalf("a holder killed from outside reads %v", doc)
	}
	if doc["conversation"] != "conv-9" {
		t.Fatalf("the conversation was lost with the holder: %v", doc)
	}
	if strings.Contains(tm.printed(), "line-0200") {
		t.Fatal("the count finished, so the holder was not killed mid-output")
	}

	other := p.attach(id2, p.bob, "")
	other.waitType("hello")
	other.typeIn("still\r")
	other.waitOut("got:still")
	if d := p.describe(id2); d["ended"] != false || d["state"] != "live" || !alive(pid2) {
		t.Fatalf("the session beside it was touched: %v", d)
	}

	// Nothing new: the same two records, in the states their holders left,
	// and the same two sessions listed.
	time.Sleep(2 * holdRedial)
	want := map[string]holdwire.RecordState{id: holdwire.StateInterrupted, id2: holdwire.StateLive}
	records := holdwire.Scan(p.terminals())
	for _, r := range records {
		if state, ok := want[r.ID]; !ok || r.State != state {
			t.Errorf("record %s reads %s: %+v", r.ID, r.State, r.Meta)
		}
	}
	if len(records) != 2 || len(p.api.everyTerminal()) != 2 {
		t.Fatalf("%d records and %d sessions listed, want the two started", len(records), len(p.api.everyTerminal()))
	}
}

// A held session is never ended for being unwatched: a long task left alone
// is what it is for.
func TestAHeldSessionIsNeverIdle(t *testing.T) {
	p := withHeld(t, nil, func(o *Options) { o.Terminal.IdleTimeout = time.Millisecond })
	doc := p.start(nil)
	id, pid := doc["id"].(string), int(doc["pid"].(float64))
	p.api.clock.set(func() time.Time { return time.Now().Add(time.Hour) })
	p.api.sweepTerminals()
	time.Sleep(300 * time.Millisecond)
	if p.describe(id)["ended"] != false || !alive(pid) {
		t.Fatalf("the idle sweep ended a held session: %v", p.describe(id))
	}
}

/* ------------------------------------------------------------ refusals --- */

// An account that is busy is refused as it is today, with the session that
// holds it named, so the person knows which one to stop.
func TestBusyNamesTheSession(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_FAIL=busy"})
	const other = "0123456789abcdef"
	dir := holdwire.Dir(p.terminals(), other)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := holdwire.WriteMeta(dir, &holdwire.Meta{ID: other, Name: "audit", Kind: "account", Account: 1, Protocol: holdwire.Major, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	release, err := holdwire.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "held by session audit ("+other+")") {
		t.Fatalf("a busy account: %d %s", resp.StatusCode, raw)
	}
}

// Each kind of failure a holder reports keeps the status the same failure
// has when this server runs the CLI itself; anything else says no more than
// that something went wrong.
func TestAHoldersRefusalKeepsItsStatus(t *testing.T) {
	for kind, code := range map[string]int{
		holdwire.KindReauth: http.StatusConflict, holdwire.KindNoAccount: http.StatusNotFound,
		holdwire.KindUnsupported: http.StatusBadGateway, holdwire.KindOutside: http.StatusBadRequest,
		holdwire.KindOther: http.StatusInternalServerError,
	} {
		t.Run(kind, func(t *testing.T) {
			p := withHeld(t, []string{"FAKE_HOLDER_FAIL=" + kind})
			resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1})
			if resp.StatusCode != code {
				t.Fatalf("%s answers %d %s, want %d", kind, resp.StatusCode, raw, code)
			}
			said := strings.Contains(string(raw), "the holder says "+kind)
			if kind == holdwire.KindOther && (said || !strings.Contains(string(raw), "internal error")) {
				t.Fatalf("an unclassified failure says %s", raw)
			}
			if kind != holdwire.KindOther && !said {
				t.Fatalf("a %s failure does not say why: %s", kind, raw)
			}
			if list := p.api.everyTerminal(); len(list) != 0 {
				t.Fatalf("a refused terminal is listed: %d", len(list))
			}
		})
	}
}

// terminal.max_sessions counts a held terminal from its request, not from
// its holder's answer: requests that come while holders are still starting
// find the places those have taken, and a holder that fails gives its place
// back.
func TestMaxSessionsCountsHoldersStillStarting(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_SLOW=500ms"}, func(o *Options) { o.Terminal.MaxSessions = 2 })
	post := func() (int, string) {
		req, _ := http.NewRequest("POST", p.srv.URL+"/v1/terminals", strings.NewReader(`{"account":1}`))
		req.Header.Set("Authorization", "Bearer "+p.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	type answer struct {
		code int
		body string
	}
	got := make(chan answer, 5)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, body := post()
			got <- answer{code, body}
		}()
	}
	wg.Wait()
	close(got)
	started, full := 0, 0
	for a := range got {
		switch {
		case a.code == http.StatusCreated:
			started++
		case a.code == http.StatusConflict && strings.Contains(a.body, "terminal.max_sessions"):
			full++
		default:
			t.Errorf("a request answered %d %s", a.code, a.body)
		}
	}
	if started != 2 || full != 3 {
		t.Fatalf("five requests at once with max_sessions 2: %d started and %d refused", started, full)
	}
	if n := len(holdwire.Scan(p.terminals())); n != 2 {
		t.Fatalf("%d holders were started for two places", n)
	}

	// A holder that fails gives its place back: with one place, each of
	// these is the holder's own refusal, never the cap.
	q := withHeld(t, []string{"FAKE_HOLDER_FAIL=busy"}, func(o *Options) { o.Terminal.MaxSessions = 1 })
	for range 2 {
		resp, raw := q.do("POST", "/v1/terminals", map[string]any{"account": 1})
		if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "the holder says busy") {
			t.Fatalf("a request after a failed start: %d %s", resp.StatusCode, raw)
		}
	}
}
