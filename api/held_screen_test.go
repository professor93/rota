//go:build linux || darwin

package api

import (
	"strings"
	"testing"
	"time"
)

// A holder says in its State whether it keeps a screen. One that was retired
// and comes back repaints every viewer of the session, by the path a viewer
// that fell behind takes; nothing else a State says repaints anybody.

func TestAScreenThatComesBackRepaintsItsViewers(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	typist := p.attach(id, p.alice, "")
	typist.waitType("hello")
	watcher := p.attach(id, p.bob, "mode=watch")
	watcher.waitType("hello")
	typist.typeIn("kept no\r")
	typist.printsUnrepainted("kept:no")
	watcher.printsUnrepainted("kept:no")

	at := time.Now()
	typist.typeIn("kept yes\r")
	for _, tm := range []*term{typist, watcher} {
		doc := tm.waitType("repaint")
		if doc["cols"] == nil || doc["rows"] == nil || doc["offset"] == nil {
			t.Fatalf("the repaint is %v", doc)
		}
		if first := tm.nextBinary(); first != "SCREEN" {
			t.Fatalf("after the repaint came %q, not the screen", first)
		}
		if took := time.Since(at); took > time.Second {
			t.Fatalf("the viewer was repainted %v after the screen came back", took)
		}
	}

	// A screen kept still has not come back.
	typist.typeIn("kept yes\r")
	typist.typeIn("again\r")
	typist.printsUnrepainted("got:again")
	watcher.printsUnrepainted("got:again")
}

// A link's first State says what is so, and repaints nobody, even when the
// link before it said the screen was gone.
func TestAFirstStateRepaintsNobody(t *testing.T) {
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("kept no\r")
	tm.printsUnrepainted("kept:no")
	tm.typeIn("relink yes\r")
	tm.printsUnrepainted("relinked")
}

// A holder older than the field never says it keeps a screen, so its States
// repaint nobody, before a viewer attaches, after, or on a new link.
func TestAnOlderHolderNeverRepaints(t *testing.T) {
	p := withHeld(t, []string{"FAKE_HOLDER_TITLE=old"})
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("kept no\r")
	tm.printsUnrepainted("kept:no")
	tm.typeIn("relink no\r")
	tm.printsUnrepainted("relinked")
	tm.typeIn("kept no\r")
	tm.typeIn("again\r")
	tm.printsUnrepainted("got:again")
}

// printsUnrepainted reads until want is printed, and fails the test if the
// connection is sent a repaint first, or before it has been answered a ping
// sent after: the frames of a connection go out in order, so the pong
// comes after whatever was queued for it before.
func (tm *term) printsUnrepainted(want string) {
	tm.t.Helper()
	from, start := len(tm.c.log), tm.out.Len()
	for !strings.Contains(tm.out.String()[start:], want) {
		switch op, payload := tm.step(); op {
		case opClose:
			tm.t.Fatalf("the socket closed with %d before %q was printed", closeCode(payload), want)
		case opText:
			if doc := tm.c.log[len(tm.c.log)-1]; doc["type"] == "repaint" {
				tm.t.Fatalf("repainted before %q was printed: %v", want, doc)
			}
		}
	}
	tm.settled()
	for _, doc := range tm.c.log[from:] {
		if doc["type"] == "repaint" {
			tm.t.Fatalf("repainted after %q was printed: %v", want, doc)
		}
	}
}

// nextBinary is the next binary frame.
func (tm *term) nextBinary() string {
	tm.t.Helper()
	for {
		switch op, payload := tm.step(); op {
		case opBinary:
			return string(payload)
		case opClose:
			tm.t.Fatalf("the socket closed with %d before a binary frame", closeCode(payload))
		}
	}
}
