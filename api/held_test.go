//go:build linux || darwin

package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
// test starts is let go of in an orderly way first and killed outright
// after, if it is still running, so none outlives the test however it
// failed.
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

// heldPIDs is every holder the store's records name that is still the
// process it recorded, for the cleanup above: a test that stopped the
// server has nothing else to ask. A holder that has gone is left out, since
// its pid may be anybody's by now: a held lock is one only a live holder
// keeps, and otherwise the pid must still have the start time and boot it
// was recorded with.
func heldPIDs(dir string) []int {
	var out []int
	for _, r := range holdwire.Scan(filepath.Join(dir, "terminals")) {
		m := r.Meta
		if m == nil || m.HolderPID <= 0 || m.HolderPID == os.Getpid() {
			continue
		}
		if r.State == holdwire.StateLive || holdwire.SameProcess(m.HolderPID, m.HolderStart, m.BootID) {
			out = append(out, m.HolderPID)
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

// gone waits a moment for pid to have gone. A killed holder stays a zombie
// until the server's Wait reaps it, which can come a little after its
// session reads ended, and signal 0 still finds a zombie.
func gone(pid int) bool {
	for range 300 {
		if !alive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

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

// A server says whether it holds the daemon claim, and on which store: for
// its own terminals directory while it runs, for no other directory, and
// for none once it has stopped. A server without HoldCommand holds none.
func TestHoldsDaemonIsTheServersOwnClaim(t *testing.T) {
	p := withHeld(t, nil)
	if !p.api.HoldsDaemon(p.terminals()) {
		t.Fatal("a daemon does not say it holds its own store")
	}
	if p.api.HoldsDaemon(t.TempDir()) {
		t.Fatal("a daemon says it holds another directory")
	}
	p.api.Stop()
	if p.api.HoldsDaemon(p.terminals()) {
		t.Fatal("a daemon that has stopped still says it holds its store")
	}
	plain := withTerminals(t)
	if plain.api.HoldsDaemon(plain.terminals()) {
		t.Fatal("a server without HoldCommand says it holds the daemon claim")
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
// state, the holder's pid, and a name, a title, a status and the
// conversation from what the holder reports. The name is the one its record
// gives it, which the title the CLI sets never changes: the title is said
// beside it, without its spinner.
func TestAHeldSessionSaysWhoItIsAndHowItIs(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_TITLE=✳ Tidy the auth code", "FAKE_HOLDER_CONVERSATION=conv-7"})
	work := filepath.Join(p.root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	id := p.start(map[string]any{"account": 1, "cwd": work})["id"].(string)
	var doc map[string]any
	for range 500 {
		if doc = p.describe(id); doc["conversation"] == "conv-7" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for key, want := range map[string]any{
		"held": true, "state": "live", "attachable": true, "status": "running",
		"name": "work", "title": "Tidy the auth code", "conversation": "conv-7",
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

	// The name the CLI gives the conversation comes before the folder, and
	// is the record's too, so a listing from the record says the same.
	p4 := withHeld(t, []string{"FAKE_HOLDER_TITLE=✳ x", "FAKE_HOLDER_CONVERSATION_NAME=audit-trail"})
	id4 := p4.start(map[string]any{"account": 1, "cwd": filepath.Join(p4.root, "sub")})["id"].(string)
	for range 500 {
		if p4.describe(id4)["title"] == "x" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if doc := p4.describe(id4); doc["name"] != "audit-trail" || doc["title"] != "x" {
		t.Errorf("a session whose CLI named its conversation is %v, titled %v; want audit-trail, x", doc["name"], doc["title"])
	}
	if r := holdwire.Judge(p4.terminals(), id4); r.Name() != "audit-trail" {
		t.Errorf("its record names it %q", r.Name())
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

// A screen with nothing to paint is no screen: the hello does not promise a
// repaint, which would have the viewer take the first bytes after it for
// the screen, and the ring is replayed instead.
func TestAnEmptyScreenIsNoScreen(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_PRINT=0123456789", "FAKE_HOLDER_SCREEN=empty"})
	id := p.start(nil)["id"].(string)
	p.waitOffset(id, 10)
	tm := p.attach(id, p.alice, "")
	hello := tm.waitType("hello")
	if hello["offset"] != float64(0) || hello["repaint"] != nil {
		t.Fatalf("hello is %v, want the ring from 0 and no repaint", hello)
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

// A link that breaks at this end — here a frame that is not one, while the
// holder goes on as if nothing had happened — is closed, so the holder sees
// it end rather than writing into it until the next link replaces it. The
// output still comes through whole, over the link dialled after it.
func TestALinkThatBreaksHereIsClosed(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_GARBLE_AT=20"})
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("count 60\r")
	tm.waitOut("line-0060\r\n")
	var want strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&want, "line-%04d\r\n", i)
	}
	if got := tm.printed(); got != "SCREEN"+want.String() {
		t.Fatalf("after a broken link the output is not whole:\n%q", got)
	}
	holderLog := holdwire.LogPath(holdwire.Dir(p.terminals(), id))
	for range 300 {
		if raw, _ := os.ReadFile(holderLog); strings.Contains(string(raw), "the daemon closed a link") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(holderLog)
	t.Fatalf("the broken link was never closed at the daemon's end; the holder's log:\n%s", raw)
}
