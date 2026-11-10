//go:build linux || darwin

package api

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// A holder says in its State whether it keeps a screen. One that was retired
// and comes back repaints every viewer of the session, by the path a viewer
// that fell behind takes; nothing else a State says repaints anybody.

func TestAScreenThatComesBackRepaintsItsViewers(t *testing.T) {
	fakesOnly(t)
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

// The first State a server has from a holder says what is so: a screen it
// says is kept then has not come back when a later State says so again.
func TestAFirstStateRepaintsNobody(t *testing.T) {
	fakesOnly(t)
	p := withHeld(t, []string{"FAKE_HOLDER_KEPT=yes"})
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("kept yes\r")
	tm.printsUnrepainted("kept:yes")
}

// Every link of one held session goes to the same holder, so a screen that
// came back while the link was down still repaints its viewers: the new
// link's first State is heard against what the last link said.
func TestARelinkAcrossARenewRepaints(t *testing.T) {
	fakesOnly(t)
	p := withHeld(t, nil)
	id := p.start(nil)["id"].(string)
	tm := p.attach(id, p.alice, "")
	tm.waitType("hello")
	tm.typeIn("kept no\r")
	tm.printsUnrepainted("kept:no")
	tm.typeIn("relink yes\r")
	tm.waitType("repaint")
	if first := tm.nextBinary(); first != "SCREEN" {
		t.Fatalf("after the repaint came %q, not the screen", first)
	}
	tm.typeIn("again\r")
	tm.printsUnrepainted("got:again")
}

// A holder older than the field never says it keeps a screen, so its States
// repaint nobody, before a viewer attaches, after, or on a new link.
func TestAnOlderHolderNeverRepaints(t *testing.T) {
	fakesOnly(t)
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

// The wake repaintViewers queues is output holding nothing, and no viewer is
// ever sent it as a frame: here the writer is woken, as it can be, after it
// has begun the repaint the wake asked for, and takes the wake once it is no
// longer behind.
func TestAWakeIsNeverSentAsAFrame(t *testing.T) {
	var tc *termConn
	tc, frames := heldViewerFrames(t, func(*termConn) []termFrame {
		tc.queued.Add(1)
		tc.out <- termFrame{op: opBinary} // queued while catchUp waited for ts.mu
		tc.behind.Store(false)
		return []termFrame{{op: opText, data: []byte(`{"type":"repaint"}`)}, {op: opBinary, data: []byte("SCREEN"), alone: true}}
	})
	tc.behind.Store(true)
	writing(t, tc)
	next := func(want string) {
		t.Helper()
		select {
		case f := <-frames:
			if string(f.data) != want {
				t.Fatalf("sent %d %q, want %q", f.op, f.data, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%q was never sent", want)
		}
	}
	next(`{"type":"repaint"}`)
	next("SCREEN")
	// Queued behind the wake, so the writer takes the wake first.
	tc.say(map[string]any{"type": "after"})
	next(`{"type":"after"}`)

	// Nor on the way out, by a connection that is not behind.
	tc2, frames2 := heldViewerFrames(t, noRepaint)
	tc2.queued.Add(1)
	tc2.out <- termFrame{op: opBinary}
	tc2.say(map[string]any{"type": "exit"})
	tc2.stop()
	go func() { tc2.goodbye(nil); tc2.c.conn.Close() }()
	select {
	case f := <-frames2:
		if string(f.data) != `{"type":"exit"}` {
			t.Fatalf("on the way out sent %d %q first", f.op, f.data)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was sent on the way out")
	}
}

// sentFrame is one frame a test read off a viewer's socket.
type sentFrame struct {
	op   byte
	data []byte
}

// heldViewerFrames is heldViewer with its socket read frame by frame.
func heldViewerFrames(t *testing.T, catchUp func(*termConn) []termFrame) (*termConn, <-chan sentFrame) {
	t.Helper()
	server, client := net.Pipe()
	frames := make(chan sentFrame, 64)
	go func() {
		defer close(frames)
		r := bufio.NewReader(client)
		for {
			var head [2]byte
			if _, err := io.ReadFull(r, head[:]); err != nil {
				return
			}
			n := int(head[1] & 0x7f)
			if n == 126 {
				var b [2]byte
				if _, err := io.ReadFull(r, b[:]); err != nil {
					return
				}
				n = int(binary.BigEndian.Uint16(b[:]))
			}
			data := make([]byte, n)
			if _, err := io.ReadFull(r, data); err != nil {
				return
			}
			frames <- sentFrame{op: head[0] & 0x0f, data: data}
		}
	}()
	tc := newTermConn(&wsConn{conn: server, done: make(chan struct{})},
		&Principal{Name: "looker", Role: RoleWatch}, true, time.Now())
	tc.repainting("", catchUp)
	t.Cleanup(func() {
		tc.stop()
		server.Close()
		client.Close()
	})
	return tc, frames
}

// fakesOnly leaves nothing on PATH but what the harness puts there, its
// fakes, so that no real CLI can be reached.
func fakesOnly(t *testing.T) { t.Setenv("PATH", t.TempDir()) }

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
