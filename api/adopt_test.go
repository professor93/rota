//go:build linux || darwin

package api

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/internal/termenv"
)

// These are about a daemon that starts on a store where held sessions
// already are: the holders a daemon before it left running, the ones that
// stopped while no daemon was there, and the ones another rota made.

/* ------------------------------------------------------------ the harness --- */

// restart stops this server and starts another on the same store, as a
// daemon restarting does. What the new one holds is whatever the old one
// left, and whatever came and went in between.
func (p *terms) restart(tune ...func(*Options)) *terms {
	p.tt.Helper()
	p.api.Stop()
	opts := p.api.opts
	for _, f := range tune {
		f(&opts)
	}
	s, err := New(opts)
	if err != nil {
		p.tt.Fatal(err)
	}
	return p.serve(s)
}

// serve is the harness for another server on the same store.
func (p *terms) serve(s *Server) *terms {
	h := &harness{t: p.tt, handler: s.Handler(), root: p.root, dir: p.dir, token: p.token, api: s}
	h.srv = httptest.NewServer(h.handler)
	p.tt.Cleanup(s.Stop)
	p.tt.Cleanup(h.srv.Close)
	p.tt.Cleanup(h.endTerminals)
	return &terms{harness: h, tt: p.tt, watch: p.watch, alice: p.alice, bob: p.bob}
}

// holdOutside starts a holder the way something other than this server
// would have: this binary as `rota serve hold` on the store, handed its
// request with no daemon in between. It answers the session's id and the
// holder's pid, and the holder is killed when the test ends.
func holdOutside(t *testing.T, home string, env ...string) (string, int) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "serve", "hold")
	cmd.Env = termenv.Set(append(os.Environ(), append([]string{"ROTA_FAKE_HOLDER=1"}, env...)...), "ROTA_HOME", home)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	go func() { _ = cmd.Wait() }()
	id := runID()
	req := holdwire.Request{ID: id, Kind: holdwire.KindAccount, Account: 1, Cwd: filepath.Join(home, "work"), Cols: 80, Rows: 24}
	if err := json.NewEncoder(stdin).Encode(req); err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	var ready holdwire.Ready
	if err := json.NewDecoder(stdout).Decode(&ready); err != nil || !ready.OK || ready.PID != pid {
		t.Fatalf("the holder answered %+v (%v)", ready, err)
	}
	return id, pid
}

// writeRecord leaves a session's directory as a holder that has gone would
// have: its record, and no lock held. What the test does not say is filled
// in as a holder would have filled it.
func writeRecord(t *testing.T, terminals string, m holdwire.Meta) string {
	t.Helper()
	if m.ID == "" {
		m.ID = runID()
	}
	if m.Kind == "" {
		m.Kind, m.Account, m.Provider = holdwire.KindAccount, 1, "claude"
	}
	if m.Protocol == 0 {
		m.Protocol, m.Version = holdwire.Major, "fake"
	}
	if m.Started.IsZero() {
		m.Started = time.Now().Add(-3 * time.Hour)
	}
	dir := holdwire.Dir(terminals, m.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := holdwire.WriteMeta(dir, &m); err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// aged moves a path's times back, as if it had last been touched that long
// ago.
func aged(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	at := time.Now().Add(-ago)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// deadSock leaves a socket file in dir with nobody listening on it, which
// is what a holder that crashed leaves.
func deadSock(t *testing.T, dir string) string {
	t.Helper()
	path := holdwire.SockPath(dir)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	return path
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func code(n int) *int { return &n }

// list is the listing, by id.
func (p *terms) list() map[string]map[string]any {
	p.tt.Helper()
	resp, raw := p.do("GET", "/v1/terminals", nil)
	if resp.StatusCode != http.StatusOK {
		p.tt.Fatalf("listing: %d %s", resp.StatusCode, raw)
	}
	var body struct {
		Terminals []map[string]any `json:"terminals"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		p.tt.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, doc := range body.Terminals {
		out[doc["id"].(string)] = doc
	}
	return out
}

// waitFor describes a terminal until want holds of it, for at most a while.
func (p *terms) waitFor(id string, within time.Duration, want func(map[string]any) bool) map[string]any {
	p.tt.Helper()
	var doc map[string]any
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if doc = p.describe(id); want(doc) {
			return doc
		}
	}
	p.tt.Fatalf("terminal %s never came to what was waited for: %v", id, doc)
	return nil
}

// wants checks a description key by key.
func wants(t *testing.T, what string, doc map[string]any, want map[string]any) {
	t.Helper()
	for key, v := range want {
		if doc[key] != v {
			t.Errorf("%s: %s is %v, want %v (%v)", what, key, doc[key], v, doc)
		}
	}
}

/* -------------------------------------------------------- coming back --- */

// A daemon restart looks to a viewer like a brief reconnect: the session is
// live under the new daemon, and its offsets are the same offsets, so a
// viewer that says where it got to is given exactly what it missed —
// including what was printed while no daemon was there.
func TestARestartReadoptsWithOffsetsIntact(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=one", "FAKE_HOLDER_UNLINKED=two"})
	doc := p.start(nil)
	id, pid := doc["id"].(string), doc["pid"].(float64)
	p.waitOffset(id, 3)
	p.api.Stop()
	time.Sleep(500 * time.Millisecond) // the holder prints "two" with no daemon there

	q := p.restart()
	wants(t, "a readopted session", q.describe(id), map[string]any{
		"held": true, "state": "live", "attachable": true, "reachable": true, "pid": pid, "ended": false,
	})
	q.waitOffset(id, 6)
	tm := q.attach(id, q.alice, "since=3")
	if hello := tm.waitType("hello"); hello["offset"] != float64(3) {
		t.Fatalf("hello is %v, want offset 3", hello)
	}
	tm.waitOut("two")
	tm.typeIn("x\r")
	tm.waitOut("got:x")
	if got := tm.printed(); got != "twogot:x\r\n" {
		t.Fatalf("from the old offset the viewer was shown %q, want two and then the answer", got)
	}
}

// Every directory is judged on its own: one that is broken hides nothing,
// and each stopped one is listed with what stopped it.
func TestEveryRecordIsJudgedOnItsOwn(t *testing.T) {
	p := withHeld(t, nil)
	terminals := p.terminals()
	live, livePID := holdOutside(t, p.dir)
	broken := runID()
	if err := os.MkdirAll(holdwire.Dir(terminals, broken), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(holdwire.MetaPath(holdwire.Dir(terminals, broken)), []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	interrupted := writeRecord(t, terminals, holdwire.Meta{Name: "audit", Conversation: "conv-i", ConversationCwd: "/w/tree"})
	stopped := time.Now().Add(-time.Minute).Truncate(time.Second)
	ended := writeRecord(t, terminals, holdwire.Meta{Exit: code(3), Ended: stopped})

	q := p.restart()
	list := q.list()
	if len(list) != 4 {
		t.Fatalf("listed %d sessions, want 4: %v", len(list), list)
	}
	wants(t, "the broken record", list[broken], map[string]any{
		"held": true, "state": "broken", "status": "stopped", "ended": true, "attachable": false, "resumable": false,
	})
	wants(t, "the interrupted record", list[interrupted], map[string]any{
		"held": true, "state": "interrupted", "status": "stopped", "ended": true, "exit": float64(-1),
		"resumable": true, "conversation": "conv-i", "conversation_cwd": "/w/tree", "name": "audit", "label": "audit",
	})
	wants(t, "the ended record", list[ended], map[string]any{
		"held": true, "state": "ended", "status": "stopped", "ended": true, "exit": float64(3),
		"resumable": false, "ended_at": stopped.Format(time.RFC3339),
	})
	wants(t, "the live record", list[live], map[string]any{
		"held": true, "state": "live", "attachable": true, "reachable": true, "pid": float64(livePID),
	})
	if acct, _ := list[ended]["account"].(map[string]any); acct["id"] != float64(1) || acct["provider"] != "claude" {
		t.Errorf("a stopped record's account is %v", list[ended]["account"])
	}

	tm := q.attach(live, q.alice, "")
	tm.waitType("hello")
	tm.typeIn("x\r")
	tm.waitOut("got:x")
}

// A holder a rota with another protocol made is listed, never linked to,
// never attached to, and never killed by anything but somebody asking —
// and then only while its pid is still the holder its record names.
func TestAHolderOfAnotherMajorIsListedAndNeverKilled(t *testing.T) {
	p := withHeld(t, nil)
	signals := filepath.Join(t.TempDir(), "signals")
	id, pid := holdOutside(t, p.dir, "FAKE_HOLDER_MAJOR=9", "FAKE_HOLDER_VERSION=9.0.0", "FAKE_HOLDER_SIGNALS="+signals)
	q := p.restart().restart().restart() // three daemons, two of them stopped
	if !alive(pid) || !holdwire.Locked(holdwire.LockPath(holdwire.Dir(q.terminals(), id))) {
		t.Fatal("starting and stopping daemons ended a holder they could not read")
	}
	wants(t, "a holder of another major", q.describe(id), map[string]any{
		"held": true, "state": "live", "attachable": false, "made_by": "9.0.0", "ended": false, "pid": float64(pid),
	})

	want := "session " + id + " was made by rota 9.0.0, which this rota cannot attach to; stop it, or attach with that version"
	if c, resp := dialWS(t, q.harness, "/v1/terminals/"+id+"/ws", "Sec-WebSocket-Protocol", "rota, bearer."+q.alice); c != nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("attaching answers %d", resp.StatusCode)
	}
	if resp, raw := q.do("GET", "/v1/terminals/"+id+"/ws", nil); resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), want) {
		t.Fatalf("attaching answers %d %s", resp.StatusCode, raw)
	}

	// A record that no longer names the process running under its pid:
	// nothing is signalled.
	dir := holdwire.Dir(q.terminals(), id)
	m, err := holdwire.ReadMeta(dir)
	if err != nil {
		t.Fatal(err)
	}
	real := m.HolderStart
	m.HolderStart = real.Add(time.Hour)
	if err := holdwire.WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	resp, raw := q.do("DELETE", "/v1/terminals/"+id, nil)
	if resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(raw), "session "+id+" is no longer the process that started it; nothing was signalled") {
		t.Fatalf("DELETE of a record an hour off answers %d %s", resp.StatusCode, raw)
	}
	time.Sleep(200 * time.Millisecond)
	if exists(signals) || !alive(pid) {
		t.Fatal("a process the record does not name was signalled")
	}

	// The record put right, a DELETE hangs it up.
	m.HolderStart = real
	if err := holdwire.WriteMeta(dir, m); err != nil {
		t.Fatal(err)
	}
	if resp, raw := q.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE answers %d %s", resp.StatusCode, raw)
	}
	doc := q.waitFor(id, 3*time.Second, func(d map[string]any) bool { return d["state"] == "ended" })
	if raw, _ := os.ReadFile(signals); strings.TrimSpace(string(raw)) != "hangup" {
		t.Fatalf("the holder was sent %q, want one hangup", raw)
	}
	wants(t, "a foreign holder that was stopped", doc, map[string]any{"exit": float64(129), "status": "stopped"})
}

// One that will not stop is killed — once it has had the time a holder has
// to stop its own CLI, and only if its lock is still held then.
func TestAHolderOfAnotherMajorThatWillNotStopIsKilled(t *testing.T) {
	wasKill, wasLater := termKillAfter, holdKillLater
	termKillAfter, holdKillLater = 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { termKillAfter, holdKillLater = wasKill, wasLater })
	p := withHeld(t, nil)
	id, pid := holdOutside(t, p.dir, "FAKE_HOLDER_MAJOR=9", "FAKE_HOLDER_DEAF=1")
	q := p.restart()
	asked := time.Now()
	if resp, raw := q.do("DELETE", "/v1/terminals/"+id, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE answers %d %s", resp.StatusCode, raw)
	}
	// However slow the machine, it is never killed sooner than its time:
	// the kill waits that long from the hangup, which came after asking.
	for deadline := time.Now().Add(10 * time.Second); alive(pid); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the holder outlived its kill")
		}
	}
	if took, after := time.Since(asked), termKillAfter+holdKillLater; took < after {
		t.Fatalf("the holder was killed %v after the DELETE, before its %v to stop", took, after)
	}
	doc := q.waitFor(id, 3*time.Second, func(d map[string]any) bool { return d["ended"] == true })
	wants(t, "a killed holder", doc, map[string]any{"state": "interrupted", "exit": float64(-1)})
}

// A live holder whose link fails is listed live, and not reachable, rather
// than taken for a holder that is gone — and it is tried again at every
// sweep, said once in the log however many times it fails.
func TestALiveHolderThatWillNotLinkIsListedAndTriedAgain(t *testing.T) {
	var log logBuf
	p := withHeld(t, nil)
	id, pid := holdOutside(t, p.dir, "FAKE_HOLDER_REFUSE_LINKS=2", "FAKE_HOLDER_PRINT=hello")
	q := p.restart(func(o *Options) { o.Log = slog.New(slog.NewTextHandler(&log, nil)) })
	wants(t, "a holder that refused the link", q.describe(id), map[string]any{
		"held": true, "state": "live", "reachable": false, "ended": false, "pid": float64(pid),
	})
	if resp, raw := q.do("GET", "/v1/terminals/"+id+"/ws", nil); resp.StatusCode != http.StatusConflict ||
		!strings.Contains(string(raw), "is not answering") {
		t.Fatalf("attaching to it answers %d %s", resp.StatusCode, raw)
	}
	if n := len(q.api.everyTerminal()); n != 0 {
		t.Fatalf("%d sessions are held after a failed link", n)
	}
	q.api.sweepRecords()
	if n := strings.Count(log.String(), "could not be linked"); n != 1 {
		t.Fatalf("two failed links were logged %d times:\n%s", n, log.String())
	}
	q.api.sweepRecords()
	wants(t, "the same holder a sweep later", q.describe(id), map[string]any{"state": "live", "reachable": true})
	tm := q.attach(id, q.alice, "since=0")
	tm.waitOut("hello")
}

// max_sessions counts every running session on the store, the ones this
// daemon took up and the ones it cannot read alike.
func TestMaxSessionsCountsWhatAnEarlierDaemonLeft(t *testing.T) {
	p := withHeld(t, nil, func(o *Options) { o.Terminal.MaxSessions = 2 })
	p.start(nil)
	holdOutside(t, p.dir, "FAKE_HOLDER_MAJOR=9")
	q := p.restart()
	resp, raw := q.do("POST", "/v1/terminals", map[string]any{"account": 1})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(raw), "holding 2 terminals") {
		t.Fatalf("a third session answers %d %s", resp.StatusCode, raw)
	}
}

// What a recorded session printed before a restart stays in its recording
// as it was, and what it prints after is added to it, once — though the
// holder sends the new daemon all it still has, from the start.
func TestARecordingCarriesOnAcrossARestart(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=one", "FAKE_HOLDER_UNLINKED=two"}, func(o *Options) { o.Terminal.Record = true })
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 3)
	p.api.Stop()
	// Told apart from what the holder sends again: the recording is the
	// one thing a restart must not rewrite.
	out := filepath.Join(p.terminals(), id+".out")
	if raw, err := os.ReadFile(out); err != nil || string(raw) != "one" {
		t.Fatalf("before the restart the recording is %q (%v)", raw, err)
	}
	if err := os.WriteFile(out, []byte("ONE"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	q := p.restart()
	q.waitOffset(id, 6)
	tm := q.attach(id, q.alice, "since=6")
	tm.waitType("hello")
	tm.typeIn("exit 0\r")
	tm.waitType("exit")
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "ONEtwo" {
		t.Fatalf("the recording is %q, want what it had and then two", raw)
	}
}

// A recording carries on from where in the output it had got to, not from
// how long its file is: what a holder kept less of than a daemon missed is
// written down as skipped, and what it sends again is never recorded
// twice, however many restarts there are.
func TestARecordingSaysWhereItSkippedAndNeverRepeats(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_KEEP=4", "FAKE_HOLDER_PRINT=one", "FAKE_HOLDER_UNLINKED=twothree"},
		func(o *Options) { o.Terminal.Record = true })
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 3)
	p.api.Stop()
	// With no daemon there the holder prints eight bytes and keeps the last
	// four: "hree", from offset 7. Offsets 3 to 7 reach no daemon.
	waitLogged(t, holdwire.LogPath(holdwire.Dir(p.terminals(), id)), "printed with nobody linked")
	out, gaps := filepath.Join(p.terminals(), id+".out"), filepath.Join(p.terminals(), id+".gaps")
	q := p.restart()
	q.waitOffset(id, 11)
	if raw, _ := os.ReadFile(out); string(raw) != "onehree" {
		t.Fatalf("the recording is %q, want one and then what the holder still had", raw)
	}
	if raw, err := os.ReadFile(gaps); string(raw) != "3 7\n" {
		t.Fatalf("the skip is written down as %q (%v), want the file's byte 3 going on with offset 7", raw, err)
	}

	r := q.restart()
	r.waitOffset(id, 11)
	tm := r.attach(id, r.alice, "since=11")
	tm.waitType("hello")
	tm.typeIn("x\r")
	tm.waitOut("got:x")
	if raw, _ := os.ReadFile(out); string(raw) != "onehreegot:x\r\n" {
		t.Fatalf("after another restart the recording is %q, want hree once and then the answer", raw)
	}
	if raw, _ := os.ReadFile(gaps); string(raw) != "3 7\n" {
		t.Fatalf("the skips are %q, want the one", raw)
	}
}

// waitLogged waits for a line in a holder's log.
func waitLogged(t *testing.T, path, line string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if raw, _ := os.ReadFile(path); strings.Contains(string(raw), line) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never said %q", path, line)
		}
	}
}

// A terminal whose start loses the race with Stop — its holder linked to
// just as the server let go of everything it holds — is let go of in turn,
// still running, rather than kept by a server that has stopped and that
// would go on dialling it, and taking its link from the next daemon, for
// as long as the process lasts.
func TestAStartThatLosesTheRaceWithStopIsLetGoOf(t *testing.T) {
	var (
		p    *terms
		next *Server
		err  error
		once sync.Once
	)
	p = withHeld(t, nil, func(o *Options) {
		hold := o.HoldCommand
		o.HoldCommand = func() *exec.Cmd {
			// Stop comes while the holder starts, and the daemon after this
			// one is in this same process, so the holder takes the link.
			once.Do(func() {
				p.api.Stop()
				next, err = New(p.api.opts)
			})
			return hold()
		}
	})
	resp, raw := p.do("POST", "/v1/terminals", map[string]any{"account": 1})
	if err != nil {
		t.Fatal(err)
	}
	q := p.serve(next)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(raw), "goes on running") {
		t.Fatalf("a start that lost the race with Stop answers %d %s", resp.StatusCode, raw)
	}
	if n := len(p.api.everyTerminal()); n != 0 {
		t.Fatalf("a stopped server holds %d terminals", n)
	}
	records := holdwire.Scan(q.terminals())
	if len(records) != 1 || records[0].State != holdwire.StateLive {
		t.Fatalf("the records are %+v, want the one holder, still running", records)
	}
	id := records[0].ID
	q.api.sweepRecords()
	wants(t, "the session the next daemon took up", q.describe(id), map[string]any{"state": "live", "reachable": true})
	time.Sleep(2 * holdRedial) // a stopped server dialling it would have taken the link by now
	tm := q.attach(id, q.alice, "")
	tm.waitType("hello")
	tm.typeIn("x\r")
	tm.waitOut("got:x")
}
