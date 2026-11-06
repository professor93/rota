//go:build linux || darwin

package api

import (
	"net"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/holdwire"
)

// These are about the held sessions a daemon finds stopped, or finds in a
// state no session should be left in: what stays listed, for how long, and
// what is cleared — never anything whose lock is held. The harness is
// adopt_test.go's.

/* ------------------------------------------------------ stopped sessions --- */

// A stopped session stays listed for keep_stopped and is then forgotten,
// directory and all — never one that is locked, whatever its age.
func TestStoppedSessionsStayForKeepStopped(t *testing.T) {
	p := withHeld(t, nil, func(o *Options) { o.Terminal.KeepStopped = time.Hour })
	terminals := p.terminals()
	old := writeRecord(t, terminals, holdwire.Meta{Exit: code(0), Ended: time.Now().Add(-2 * time.Hour)})
	recent := writeRecord(t, terminals, holdwire.Meta{Exit: code(0), Ended: time.Now().Add(-30 * time.Minute)})
	oldInterrupted := writeRecord(t, terminals, holdwire.Meta{})
	aged(t, holdwire.MetaPath(holdwire.Dir(terminals, oldInterrupted)), 2*time.Hour)
	// No record at all: a holder that never got as far as writing one.
	stray, young, locked := runID(), runID(), runID()
	for _, id := range []string{stray, young, locked} {
		if err := os.MkdirAll(holdwire.Dir(terminals, id), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	release, err := holdwire.Lock(holdwire.Dir(terminals, locked))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	aged(t, holdwire.Dir(terminals, stray), 2*time.Hour)
	aged(t, holdwire.Dir(terminals, locked), 2*time.Hour)

	for range 2 {
		p.api.sweepRecords()
	}
	for id, want := range map[string]bool{old: false, oldInterrupted: false, stray: false, recent: true, young: true, locked: true} {
		if got := exists(holdwire.Dir(terminals, id)); got != want {
			t.Errorf("%s: there is %v, want %v", id, got, want)
		}
	}
	list := p.list()
	if _, ok := list[recent]; !ok || len(list) != 1 {
		t.Fatalf("the listing is %v, want the recent record alone", list)
	}
}

// After the holder has gone, the session is over: it says interrupted, keeps
// its conversation so it can be resumed, and nothing starts again by itself.
// Every other session goes on as it was (Review Focus 1).
func TestAKilledHolderReadsInterrupted(t *testing.T) {
	var spawned atomic.Int32
	p := withHeld(t, []string{"FAKE_HOLDER_CONVERSATION=conv-9", "FAKE_HOLDER_CONVERSATION_CWD=/w/tree"}, func(o *Options) {
		hold := o.HoldCommand
		o.HoldCommand = func() *exec.Cmd { spawned.Add(1); return hold() }
	})
	one := p.start(nil)
	id, pid := one["id"].(string), int(one["pid"].(float64))
	other := p.start(nil)["id"].(string)
	p.waitFor(id, 5*time.Second, func(d map[string]any) bool { return d["conversation"] == "conv-9" })
	tm := p.attach(other, p.alice, "")
	tm.waitType("hello")

	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	doc := p.waitFor(id, 2*time.Second, func(d map[string]any) bool { return d["state"] == "interrupted" })
	wants(t, "a killed holder's session", doc, map[string]any{
		"ended": true, "exit": float64(-1), "status": "stopped", "conversation": "conv-9",
		"conversation_cwd": "/w/tree", "resumable": true, "reachable": false,
	})

	tm.typeIn("still\r")
	tm.waitOut("got:still")
	p.api.sweepRecords()
	time.Sleep(200 * time.Millisecond)
	if n := spawned.Load(); n != 2 {
		t.Fatalf("%d holders were started, want the 2 asked for", n)
	}
	if records := holdwire.Scan(p.terminals()); len(records) != 2 {
		t.Fatalf("%d records, want 2", len(records))
	}
	// And once this server has forgotten it, the record says the same.
	p.api.dropTerminal(id)
	wants(t, "the record of a killed holder", p.describe(id), map[string]any{
		"held": true, "state": "interrupted", "ended": true, "conversation": "conv-9",
		"conversation_cwd": "/w/tree", "resumable": true,
	})
}

// A crashed holder's socket goes at the next sweep. A session whose lock is
// held keeps its socket through every sweep, and one that holds its lock
// while its socket says nothing is still a live session — not one that has
// gone (Review Focus 2).
func TestAStaleSockIsClearedOnlyWhenUnlocked(t *testing.T) {
	wasShake := holdHandshake
	holdHandshake = 200 * time.Millisecond
	t.Cleanup(func() { holdHandshake = wasShake })
	p := withHeld(t, nil)
	terminals := p.terminals()

	crashed := writeRecord(t, terminals, holdwire.Meta{})
	crashedSock := deadSock(t, holdwire.Dir(terminals, crashed))

	starting := runID() // locked, and listening, before its record is written
	startingDir := holdwire.Dir(terminals, starting)
	if err := os.MkdirAll(startingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := holdwire.Lock(startingDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	ln, err := net.Listen("unix", holdwire.SockPath(startingDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	aged(t, startingDir, 2*time.Hour)

	silent := writeRecord(t, terminals, holdwire.Meta{HolderPID: os.Getpid()}) // locked, never answers
	silentDir := holdwire.Dir(terminals, silent)
	releaseSilent, err := holdwire.Lock(silentDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseSilent)
	quiet, err := net.Listen("unix", holdwire.SockPath(silentDir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quiet.Close() })

	for range 3 {
		p.api.sweepRecords()
	}
	if exists(crashedSock) {
		t.Error("a crashed holder's socket is still there")
	}
	if !exists(holdwire.MetaPath(holdwire.Dir(terminals, crashed))) {
		t.Error("clearing the socket took the record with it")
	}
	for _, path := range []string{holdwire.SockPath(startingDir), holdwire.SockPath(silentDir)} {
		if !exists(path) {
			t.Errorf("%s was deleted under a held lock", path)
		}
	}
	wants(t, "a holder that says nothing", p.describe(silent), map[string]any{
		"state": "live", "reachable": false, "ended": false,
	})
	if _, ok := p.list()[starting]; ok {
		t.Error("a directory with no record yet is listed")
	}
}

// An ended session is still described after this server has stopped
// keeping it, from its record: its state and its exit (Review Focus 5).
func TestAnEndedSessionStaysListed(t *testing.T) {
	was := keepEnded
	keepEnded = 100 * time.Millisecond
	t.Cleanup(func() { keepEnded = was })
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("exit 0\r")
	tm.waitType("exit")
	held := p.waitFor(id, 2*time.Second, func(d map[string]any) bool { return d["ended"] == true })
	wants(t, "an ended session still held here", held, map[string]any{
		"state": "ended", "exit": float64(0), "status": "stopped", "reachable": false, "resumable": false,
	})
	if _, ok := held["ended_at"].(string); !ok {
		t.Errorf("an ended session says no ended_at: %v", held)
	}
	for deadline := time.Now().Add(3 * time.Second); p.api.findTerminal(id) != nil; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the ended session was never let go of")
		}
	}
	doc := p.waitFor(id, 2*time.Second, func(d map[string]any) bool { return d["state"] == "ended" })
	wants(t, "an ended session after keepEnded", doc, map[string]any{
		"held": true, "ended": true, "exit": float64(0), "status": "stopped", "attachable": false,
	})
	if _, ok := p.list()[id]; !ok {
		t.Fatal("the ended session is not in the listing")
	}
}

// A holder killed before it said Ready — the daemon gives up on one after
// holdReady, perhaps in the middle of preparing the account — leaves its
// directory with a lock file and no record. It is not a session, so it is
// never listed; it is cleared once it is an hour old, and only while
// nobody holds its lock.
func TestAHolderKilledBeforeItsRecordIsClearedOnlyWhenUnlocked(t *testing.T) {
	p := withHeld(t, nil)
	terminals := p.terminals()
	killed, holding := runID(), runID()
	for _, id := range []string{killed, holding} {
		dir := holdwire.Dir(terminals, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		release, err := holdwire.Lock(dir)
		if err != nil {
			t.Fatal(err)
		}
		if id == killed {
			release() // as the kernel does for a process it has killed
		} else {
			t.Cleanup(release)
		}
		if err := os.WriteFile(holdwire.LogPath(dir), []byte("preparing the account\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p.api.sweepRecords()
	if !exists(holdwire.Dir(terminals, killed)) {
		t.Fatal("a directory with no record was cleared before it was an hour old")
	}
	list := p.list()
	if _, ok := list[killed]; ok {
		t.Fatalf("a directory with no record is listed: %v", list[killed])
	}

	for _, id := range []string{killed, holding} {
		aged(t, holdwire.Dir(terminals, id), 2*time.Hour)
	}
	p.api.sweepRecords()
	if exists(holdwire.Dir(terminals, killed)) {
		t.Error("an hour-old directory nobody holds, with no record, is still there")
	}
	for _, path := range []string{holdwire.LockPath(holdwire.Dir(terminals, holding)), holdwire.LogPath(holdwire.Dir(terminals, holding))} {
		if !exists(path) {
			t.Errorf("%s went from under a held lock", path)
		}
	}
}
