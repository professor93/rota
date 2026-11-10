//go:build linux || darwin

package api

import (
	"encoding/json"
	"testing"
	"time"
)

// These are about the state frame: a held session telling everyone
// watching it how it is — running, needing its person or quiet, what it
// waits for, its title and its name — when they attach and whenever any of
// that changes, so a page need not poll to find out.

// afterHello is the first document a connection is sent after its hello and
// the keyboard frame, if one comes.
func (tm *term) afterHello() map[string]any {
	tm.t.Helper()
	tm.waitType("hello")
	return tm.waitDoc("a frame after hello", func(doc map[string]any) bool { return doc["type"] != "keyboard" })
}

// states is every state frame a connection has been sent so far.
func (tm *term) states() []map[string]any {
	var out []map[string]any
	for _, doc := range tm.c.log {
		if doc["type"] == "state" {
			out = append(out, doc)
		}
	}
	return out
}

// textsFor is every document a connection is sent within d, read on a
// goroutine of its own so a silence is an answer. The connection is closed
// after.
func (tm *term) textsFor(d time.Duration) []map[string]any {
	got := make(chan []map[string]any)
	go func() {
		var docs []map[string]any
		for {
			op, payload, err := readFrame(tm.c)
			if err != nil {
				got <- docs
				return
			}
			var doc map[string]any
			if op == opText && json.Unmarshal(payload, &doc) == nil {
				docs = append(docs, doc)
			}
		}
	}()
	time.Sleep(d)
	tm.c.conn.Close()
	return <-got
}

// waitTitle waits for a held session's description to carry a title, so an
// attach after it is shown that title from its first state frame.
func (p *terms) waitTitle(id, title string) {
	p.tt.Helper()
	for range 500 {
		if p.describe(id)["title"] == title {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.tt.Fatalf("terminal %s never had the title %q: %v", id, title, p.describe(id))
}

func wantState(t *testing.T, who string, doc map[string]any, want map[string]any) {
	t.Helper()
	if doc["type"] != "state" {
		t.Fatalf("%s was sent %v, want a state frame", who, doc)
	}
	for key, v := range want {
		if doc[key] != v {
			t.Errorf("%s: the state's %s is %v, want %v (%v)", who, key, doc[key], v, doc)
		}
	}
}

func TestAHeldSessionSaysItsStateAfterHello(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_TITLE=✳ fix"})
	id := p.start(map[string]any{"account": 1, "label": "audit"})["id"].(string)
	p.waitTitle(id, "fix")
	tm := p.attach(id, p.alice, "")
	wantState(t, "an attach", tm.afterHello(), map[string]any{
		"status": "running", "waiting_for": "", "title": "fix", "name": "audit",
	})
	// A watcher, who takes no keyboard, is told the same.
	w := p.attach(id, p.watch, "")
	wantState(t, "a watcher", w.afterHello(), map[string]any{
		"status": "running", "waiting_for": "", "title": "fix", "name": "audit",
	})
}

func TestAStateChangeReachesEveryViewer(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	alice := p.attach(id, p.alice, "")
	alice.afterHello()
	bob := p.attach(id, p.bob, "")
	bob.afterHello()

	alice.typeIn("state permission Approve?\r")
	want := map[string]any{"status": "needs_you", "waiting_for": "permission", "title": "Approve?"}
	for who, tm := range map[string]*term{"the holder of the keyboard": alice, "another viewer": bob} {
		doc := tm.waitDoc("a state that needs you", func(doc map[string]any) bool {
			return doc["type"] == "state" && doc["status"] == "needs_you"
		})
		wantState(t, who, doc, want)
	}
	doc := p.describe(id)
	if doc["waiting_for"] != "permission" || doc["status"] != "needs_you" {
		t.Errorf("the description says waiting_for %v and status %v, want permission and needs_you",
			doc["waiting_for"], doc["status"])
	}
}

func TestAnUnchangedStateIsNotSaidAgain(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.afterHello()
	before := len(tm.states())

	tm.typeIn("state permission Approve?\r")
	tm.typeIn("state permission Approve?\r")
	tm.typeIn("state permission Approve the edit?\r")
	tm.waitDoc("the second title", func(doc map[string]any) bool {
		return doc["type"] == "state" && doc["title"] == "Approve the edit?"
	})
	tm.settled()
	got := tm.states()[before:]
	if len(got) != 2 || got[0]["title"] != "Approve?" {
		t.Fatalf("one state said twice and then a new title gave %d frames, want 2: %v", len(got), got)
	}
}

// Quiet is said by no frame from the holder, only by time: each beat asks
// tellState, which says it once it is so, and output tells everyone at once,
// without waiting for a beat, that it is over. The beat is stood in for by
// calling tellState here: holdBeat is not shortened, since a goroutine an
// earlier test left behind may still be reading it.
func TestQuietIsSaidAndSoIsWakingUp(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	wantState(t, "an attach", tm.afterHello(), map[string]any{"status": "running"})

	p.api.clock.set(func() time.Time { return time.Now().Add(31 * time.Second) })
	t.Cleanup(func() { p.api.clock.set(nil) })
	p.api.findTerminal(id).tellState()
	tm.waitDoc("a quiet state", func(doc map[string]any) bool {
		return doc["type"] == "state" && doc["status"] == "quiet"
	})
	typed := time.Now()
	tm.typeIn("x\r")
	tm.waitDoc("a running state", func(doc map[string]any) bool {
		return doc["type"] == "state" && doc["status"] == "running"
	})
	if took := time.Since(typed); took > holdBeat/2 {
		t.Errorf("output ended quiet only %v later, at a beat rather than at once", took)
	}
}

// A session that has stopped waits for nothing, whatever its registry said
// last.
func TestAStoppedSessionWaitsForNothing(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_WAITING=permission"})
	id := p.start(nil)["id"].(string)
	p.waitFor(id, 5*time.Second, func(doc map[string]any) bool { return doc["waiting_for"] == "permission" })
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("exit 0\r")
	if doc := p.waitEnded(id); doc["waiting_for"] != "" {
		t.Errorf("a stopped session says it waits for %v, want nothing", doc["waiting_for"])
	}
}

func TestATerminalThatIsNotHeldSaysNoState(t *testing.T) {
	p := withTerminals(t)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	for _, doc := range tm.textsFor(300 * time.Millisecond) {
		if doc["type"] == "state" {
			t.Fatalf("a terminal this server runs itself said a state: %v", doc)
		}
	}
}
