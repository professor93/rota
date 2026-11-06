//go:build linux || darwin

package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// These are about a held terminal: one whose CLI lives in a holder process
// of its own rather than in the server, so that it outlives the server. The
// holder here is this test binary playing one (fakeholder_test.go), told by
// its environment how to behave, so what is tested is the server's side of
// the link and nothing about a real CLI.

/* ------------------------------------------------------------ the harness --- */

// withHeld is withTerminals with HoldCommand set: every terminal is a fake
// holder started from this binary. env is the fake's script. Every holder a
// test starts is ended in an orderly way first and killed outright after, so
// none outlives the test however it failed.
func withHeld(t *testing.T, env []string, tune ...func(*Options)) *terms {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	hold := func(o *Options) {
		o.HoldCommand = func() *exec.Cmd {
			cmd := exec.Command(exe, "serve", "hold")
			cmd.Env = append(os.Environ(), append([]string{"ROTA_FAKE_HOLDER=1"}, env...)...)
			return cmd
		}
	}
	p := withTerminals(t, append([]func(*Options){hold}, tune...)...)
	t.Cleanup(func() {
		p.harness.endTerminals()
		for _, pid := range heldPIDs(p.dir) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return p
}

// heldPIDs is every holder pid the store's records name, for the cleanup
// above: a test that stopped the server has nothing else to ask.
func heldPIDs(dir string) []int {
	var out []int
	for _, r := range holdwire.Scan(filepath.Join(dir, "terminals")) {
		if r.Meta != nil && r.Meta.HolderPID > 0 && r.Meta.HolderPID != os.Getpid() {
			out = append(out, r.Meta.HolderPID)
		}
	}
	return out
}

func (p *terms) terminals() string { return filepath.Join(p.dir, "terminals") }

// waitOffset waits for the server to have taken in n bytes of a terminal's
// output, so a test about what an attach is shown starts from a known place.
func (p *terms) waitOffset(id string, n int) {
	p.tt.Helper()
	for range 500 {
		if p.describe(id)["offset"] == float64(n) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.tt.Fatalf("terminal %s never reached offset %d: %v", id, n, p.describe(id))
}

// waitEnded waits for a terminal to be described as ended.
func (p *terms) waitEnded(id string) map[string]any {
	p.tt.Helper()
	for range 800 {
		if doc := p.describe(id); doc["ended"] == true {
			return doc
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.tt.Fatalf("terminal %s never ended: %v", id, p.describe(id))
	return nil
}

// binaries reads frames until the bytes shown after the first binary frame
// come to want, and returns that first frame and what followed it.
func (tm *term) binaries(want int) (first string, rest string) {
	tm.t.Helper()
	got := false
	var after strings.Builder
	for range 4000 {
		op, payload := tm.step()
		switch op {
		case opBinary:
			if !got {
				first, got = string(payload), true
			} else {
				after.Write(payload)
			}
		case opClose:
			tm.t.Fatalf("closed with %d; first %q then %q", closeCode(payload), first, after.String())
		}
		if got && after.Len() >= want {
			return first, after.String()
		}
	}
	tm.t.Fatalf("only %q then %q arrived", first, after.String())
	return "", ""
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

/* ------------------------------------------------------- living elsewhere --- */

// A held terminal's CLI is not this server's process, and stopping the
// server lets go of it rather than ending it: the holder is still alive, its
// session still locked, and the daemon's own claim on the store is free for
// the next daemon.
func TestAHeldTerminalLivesInItsOwnProcessAndSurvivesStop(t *testing.T) {
	p := withHeld(t, nil)
	doc := p.start(map[string]any{"account": 1, "cols": 120, "rows": 40, "label": "fintech"})
	id, _ := doc["id"].(string)
	pid := int(doc["pid"].(float64))
	if doc["held"] != true || pid <= 0 || pid == os.Getpid() {
		t.Fatalf("a held terminal is held, by a process of its own: %v", doc)
	}
	if !alive(pid) {
		t.Fatalf("holder %d is not running", pid)
	}
	terminals := p.terminals()
	if !holdwire.Locked(holdwire.DaemonLock(terminals)) {
		t.Fatal("a server holding terminals holds its store's daemon lock")
	}

	p.api.Stop()
	time.Sleep(200 * time.Millisecond) // long enough for a holder that took the stop as its end to go
	if !alive(pid) {
		t.Fatal("stopping the server ended the holder")
	}
	if !holdwire.Locked(holdwire.LockPath(holdwire.Dir(terminals, id))) {
		t.Fatal("the session's lock was let go of, so it reads as stopped")
	}
	if holdwire.Locked(holdwire.DaemonLock(terminals)) {
		t.Fatal("daemon.lock is still held after Stop")
	}
	if p.api.findTerminal(id) != nil {
		t.Error("a terminal let go of is still listed by the server that let go")
	}
}

// One store has one daemon. A second server with HoldCommand on the same
// directory is refused and told which process serves it.
func TestTheDaemonClaimIsExclusive(t *testing.T) {
	p := withHeld(t, nil)
	_, err := New(Options{Token: "x", Dir: p.dir, RefreshEvery: -1, HoldCommand: p.api.opts.HoldCommand})
	if err == nil {
		t.Fatal("a second daemon claimed the same store")
	}
	want := fmt.Sprintf("api: another rota daemon serves %s (pid %d)", p.dir, os.Getpid())
	if err.Error() != want {
		t.Fatalf("the refusal is %q, want %q", err, want)
	}
}

// Natural by default: a server without HoldCommand runs its terminals itself
// as it always did, claims nothing, and describes nothing new.
func TestWithoutHoldCommandNothingChanges(t *testing.T) {
	p := withTerminals(t)
	doc := p.start(nil)
	for _, key := range []string{"held", "state", "attachable", "pid", "name", "title", "conversation", "status"} {
		if _, ok := doc[key]; ok {
			t.Errorf("a terminal this server runs describes %q: %v", key, doc[key])
		}
	}
	if _, err := os.Stat(holdwire.DaemonLock(p.terminals())); !os.IsNotExist(err) {
		t.Errorf("a server without HoldCommand wrote daemon.lock: %v", err)
	}
	hello := p.attach(doc["id"].(string), p.alice, "").waitType("hello")
	if _, ok := hello["repaint"]; ok {
		t.Errorf("hello says repaint: %v", hello)
	}
}

/* ------------------------------------------------------------- what it is --- */

// The description says what only a held session has: that it is held, its
// state, the holder's pid, and a name, a status and the conversation from
// what the holder reports.
func TestAHeldSessionSaysWhoItIsAndHowItIs(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_TITLE=✳ Tidy the auth code", "FAKE_HOLDER_CONVERSATION=conv-7"})
	id := p.start(nil)["id"].(string)
	var doc map[string]any
	for range 500 {
		if doc = p.describe(id); doc["conversation"] == "conv-7" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for key, want := range map[string]any{
		"held": true, "state": "live", "attachable": true, "status": "running",
		"name": "Tidy the auth code", "title": "✳ Tidy the auth code", "conversation": "conv-7",
	} {
		if doc[key] != want {
			t.Errorf("%s is %v, want %v", key, doc[key], want)
		}
	}
	p.api.clock.set(func() time.Time { return time.Now().Add(31 * time.Second) })
	if got := p.describe(id)["status"]; got != "quiet" {
		t.Errorf("thirty seconds without output reads %v, want quiet", got)
	}
	p.api.clock.set(nil)

	// A label comes before the title, and a session that waits on its person
	// says so.
	p2 := withHeld(t, []string{"FAKE_HOLDER_TITLE=✳ x", "FAKE_HOLDER_WAITING=permission"})
	id2 := p2.start(map[string]any{"account": 1, "label": "audit"})["id"].(string)
	for range 500 {
		if p2.describe(id2)["status"] == "needs_you" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if doc := p2.describe(id2); doc["status"] != "needs_you" || doc["name"] != "audit" {
		t.Errorf("a labelled session waiting on a permission: %v", doc)
	}

	// With nothing else to go by, it is named after its folder.
	p3 := withHeld(t, nil)
	doc3 := p3.start(map[string]any{"account": 1, "cwd": filepath.Join(p3.root, "sub")})
	if doc3["name"] != "sub" {
		t.Errorf("a session with no label, title or registry is named %v, want its folder", doc3["name"])
	}
}

// The holder is told what to run on its standard input, with the store it
// belongs to in its environment, in a session of its own — and never a
// credential: it prepares the account itself.
func TestTheHolderIsToldWhatToRunAndNoSecret(t *testing.T) {
	log := filepath.Join(t.TempDir(), "request.json")
	p := withHeld(t, []string{"FAKE_HOLDER_LOG=" + log})
	doc := p.start(map[string]any{"account": 1, "cols": 100, "rows": 30, "label": "x", "args": []string{"--model", "opus"}})
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "tok-1") {
		t.Fatalf("the holder was handed the account's token: %s", raw)
	}
	var seen struct {
		Request  holdwire.Request `json:"request"`
		RotaHome string           `json:"rota_home"`
	}
	if err := json.Unmarshal(raw, &seen); err != nil {
		t.Fatal(err)
	}
	r := seen.Request
	if r.ID != doc["id"] || r.Kind != "account" || r.Account != 1 || r.Name != "x" ||
		r.Cols != 100 || r.Rows != 30 || strings.Join(r.Args, " ") != "--model opus" || r.Scrollback <= 0 {
		t.Errorf("the request is %+v", r)
	}
	if seen.RotaHome != p.dir || !filepath.IsAbs(seen.RotaHome) {
		t.Errorf("ROTA_HOME is %q, want %q", seen.RotaHome, p.dir)
	}
	// A new session is a new process group too, which both kernels report.
	pid := int(doc["pid"].(float64))
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("the holder is in process group %d (%v), not a session of its own", pgid, err)
	}
}

/* ------------------------------------------------------------ attaching --- */

// An attach that says nothing is shown the holder's screen, then the bytes
// after the offset that screen stands for — not the scrollback.
func TestAttachSendsTheScreenThenTheRest(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=0123456789", "FAKE_HOLDER_SCREEN_AT=4"})
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 10)

	tm := p.attach(id, p.alice, "")
	hello := tm.waitType("hello")
	if hello["offset"] != float64(4) || hello["repaint"] != true {
		t.Fatalf("hello is %v, want offset 4 and a repaint", hello)
	}
	first, rest := tm.binaries(len("456789"))
	if first != "SCREEN" || rest != "456789" {
		t.Fatalf("shown %q then %q, want SCREEN then 456789", first, rest)
	}
	tm.typeIn("x\r")
	tm.waitOut("got:x")

	// A viewer that says where it got to is given the bytes from there, as
	// ever, with no screen.
	other := p.attach(id, p.bob, "since=2")
	hello = other.waitType("hello")
	if hello["offset"] != float64(2) || hello["repaint"] != nil {
		t.Fatalf("hello with since is %v", hello)
	}
	other.waitOut("23456789got:x")
}

// A holder with no screen to give is replayed from the ring, as a terminal
// this server runs is.
func TestAnAttachWithoutAScreenReplaysTheRing(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=0123456789", "FAKE_HOLDER_SCREEN=none"})
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 10)
	tm := p.attach(id, p.alice, "")
	hello := tm.waitType("hello")
	if hello["offset"] != float64(0) || hello["repaint"] != nil {
		t.Fatalf("hello is %v, want the ring from 0", hello)
	}
	tm.waitOut("0123456789")
}

// A screen can stand for an offset this server has yet to be sent the bytes
// of. The viewer shown it must not be shown those bytes again when they
// arrive: they are already on its screen.
func TestAScreenAheadOfTheRingSkipsWhatItAlreadyShows(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=0123456789", "FAKE_HOLDER_SCREEN_AHEAD=abc"})
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 10)
	tm := p.attach(id, p.alice, "")
	hello := tm.waitType("hello")
	if hello["offset"] != float64(13) || hello["repaint"] != true {
		t.Fatalf("hello is %v, want offset 13 and a repaint", hello)
	}
	p.waitOffset(id, 13)
	tm.typeIn("x\r")
	first, rest := tm.binaries(len("got:x\r\n"))
	if first != "SCREEN" || rest != "got:x\r\n" {
		t.Fatalf("shown %q then %q, want SCREEN then got:x", first, rest)
	}
}

/* --------------------------------------------------------------- typing --- */

// Keystrokes the holder could not take are said to the connection that
// typed them, and to nobody else.
func TestARefusedKeystrokeIsSaidToWhoTyped(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_REFUSE=nope"})
	id := p.start(nil)["id"].(string)
	typist := p.attach(id, p.alice, "")
	typist.waitType("hello")
	watcher := p.attach(id, p.bob, "mode=watch")
	watcher.waitType("hello")

	typist.typeIn("nope")
	refused := typist.waitType("error")
	if msg, _ := refused["message"].(string); !strings.Contains(msg, "that input was not typed") {
		t.Fatalf("the refusal says %q", msg)
	}
	typist.typeIn("fine\r")
	typist.waitOut("got:fine")
	watcher.waitOut("got:fine")
	for _, doc := range watcher.c.log {
		if doc["type"] == "error" {
			t.Fatalf("somebody who typed nothing was told %v", doc)
		}
	}
}

/* -------------------------------------------------------------- the link --- */

// The link to a holder can break with the holder still there. The server
// dials it again and asks for the bytes since the last it has, so what a
// viewer is shown has nothing twice and nothing missing.
func TestALostLinkComesBackWithItsOffset(t *testing.T) {
	var log logBuf
	p := withHeld(t, []string{"FAKE_HOLDER_DROP_AT=50"}, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("count 200\r")
	tm.waitOut("line-0200\r\n")
	var want strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&want, "line-%04d\r\n", i)
	}
	// The attach was shown the fake's screen first, which is no output.
	if got := tm.printed(); got != "SCREEN"+want.String() {
		t.Fatalf("after a lost link the output is not whole:\n%q", got)
	}
	if got := p.describe(id)["offset"]; got != float64(want.Len()) {
		t.Fatalf("the offset is %v, want %d", got, want.Len())
	}
	if !strings.Contains(log.String(), "terminal relinked") {
		t.Fatalf("the link was never lost and dialled again:\n%s", log.String())
	}
}

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
	if alive(pid) {
		t.Fatal("the holder outlived its kill")
	}
	if doc["state"] != "interrupted" || doc["exit"] != float64(-1) {
		t.Fatalf("a killed holder reads %v", doc)
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
