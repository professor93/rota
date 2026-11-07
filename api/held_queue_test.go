package api

import (
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/ring"
)

// These are about one held session's viewer's queue, below the socket: what
// it keeps while it is behind, what it gives up on, and how often it is
// repainted. The tests through a real holder are in held_view_test.go; these
// say the rules the flood there cannot make happen on cue.

// heldViewer is a held session's viewer whose repaint is catchUp, writing to
// a socket that is read and thrown away.
func heldViewer(t *testing.T, catchUp func(*termConn) []termFrame) *termConn {
	t.Helper()
	server, client := net.Pipe()
	go func() { _, _ = io.Copy(io.Discard, client) }()
	tc := newTermConn(&wsConn{conn: server, done: make(chan struct{})},
		&Principal{Name: "looker", Role: RoleWatch}, true, time.Now())
	tc.repainting("", catchUp)
	t.Cleanup(func() {
		tc.stop()
		server.Close()
		client.Close()
	})
	return tc
}

func noRepaint(*termConn) []termFrame { return nil }

// writing starts a viewer's writer, which is done with before anything the
// test changed is put back.
func writing(t *testing.T, tc *termConn) {
	go tc.write()
	t.Cleanup(func() {
		tc.stop()
		<-tc.drained
	})
}

// A document is not output, and a repaint does not say it again: a viewer
// whose room for documents is full is closed as slow, with 1013, so that
// reattaching is told in its hello what it would otherwise never hear.
func TestAViewerThatCannotBeToldADocumentIsClosedAsSlow(t *testing.T) {
	tc := heldViewer(t, noRepaint) // its writer never runs: nothing leaves the queue
	for range int(tc.depth) + 1 {
		tc.push(opBinary, []byte("x"))
	}
	if !tc.behind.Load() || tc.done() {
		t.Fatalf("output past the depth: behind %v, closed %v; want behind and open", tc.behind.Load(), tc.done())
	}
	for range termDocRoom {
		tc.say(map[string]any{"type": "viewers"})
	}
	if tc.done() {
		t.Fatal("a viewer was closed while its documents still fit")
	}
	tc.say(map[string]any{"type": "keyboard"})
	if !tc.done() || !tc.slow.Load() {
		t.Fatalf("a document with no room: closed %v, slow %v; want closed as slow", tc.done(), tc.slow.Load())
	}
}

// A viewer that is behind keeps the documents that come meanwhile for after
// its repaint, up to termLate; one owed more than that is closed as slow
// rather than have the rest dropped without a word.
func TestAViewerOwedTooMuchAfterItsRepaintIsClosedAsSlow(t *testing.T) {
	tc := heldViewer(t, noRepaint)
	tc.repaintAt = time.Now().Add(time.Hour) // never repainted, so it only keeps
	tc.behind.Store(true)
	writing(t, tc)
	drained := func() {
		t.Helper()
		for range 5000 {
			if len(tc.out) == 0 || tc.done() {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("the writer never took what was queued")
	}
	for sent := 0; sent < termLate; {
		for n := 0; n < termDocRoom && sent < termLate; n++ {
			tc.say(map[string]any{"type": "viewers"})
			sent++
		}
		drained()
	}
	if tc.done() {
		t.Fatalf("a viewer owed %d documents was closed", termLate)
	}
	tc.say(map[string]any{"type": "viewers"})
	select {
	case <-tc.drained:
	case <-time.After(5 * time.Second):
		t.Fatalf("a viewer owed %d documents was kept", termLate+1)
	}
	if !tc.slow.Load() {
		t.Fatal("a viewer owed too much was closed, but not as slow")
	}
}

// A gap that comes while a viewer is behind is not sent to it: its repaint
// goes on from past the gap, and the gap's offsets, arriving after that,
// would take it backwards. A viewer that is not behind is sent the gap as
// ever, and a document that is not about the output still waits for the
// repaint.
func TestAGapIsNotSentToAViewerBehindOnIt(t *testing.T) {
	ts := &termSession{s: &Server{opts: Options{Terminal: Terminal{Scrollback: 1 << 16}}},
		id: "t", log: func(string, ...any) {}, ring: ring.New(1 << 16), cols: 80, rows: 24}
	behind, keeping := heldViewer(t, noRepaint), heldViewer(t, noRepaint)
	behind.behind.Store(true)
	ts.conns = []*termConn{behind, keeping}

	ts.outputAt(100, []byte("after the gap"))
	ts.resizedBy(100, 30)

	types := func(tc *termConn) (out []string) {
		for len(tc.out) > 0 {
			f := <-tc.out
			if f.op == opBinary {
				out = append(out, "output")
				continue
			}
			var doc map[string]any
			_ = json.Unmarshal(f.data, &doc)
			kind, _ := doc["type"].(string)
			if f.late {
				kind += " (late)"
			}
			out = append(out, kind)
		}
		return out
	}
	if got := types(behind); len(got) != 1 || got[0] != "resized (late)" {
		t.Fatalf("a viewer behind was queued %q, want only the resize, for after its repaint", got)
	}
	if got := types(keeping); len(got) != 3 || got[0] != "gap" || got[1] != "output" || got[2] != "resized" {
		t.Fatalf("a viewer in step was queued %q, want the gap, the output and the resize", got)
	}
}

// A viewer that stays slow falls behind again soon after each repaint, and
// each asks its holder for a screen: it is repainted at most once in
// termRepaintGap, however soon it falls behind again.
func TestAViewerIsRepaintedAtMostSoOften(t *testing.T) {
	was := termRepaintGap
	termRepaintGap = 100 * time.Millisecond
	t.Cleanup(func() { termRepaintGap = was })

	var mu sync.Mutex
	var at []time.Time
	tc := heldViewer(t, func(c *termConn) []termFrame {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		c.behind.Store(false)
		return []termFrame{{op: opText, data: []byte(`{"type":"repaint"}`)}}
	})
	writing(t, tc)
	repaints := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(at)
	}
	for deadline := time.Now().Add(10 * time.Second); repaints() < 4 && time.Now().Before(deadline); {
		// Behind again at once, as a viewer that stays slow is, and with
		// something queued, as falling behind always has.
		tc.behind.Store(true)
		tc.say(map[string]any{"type": "viewers"})
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 4 {
		t.Fatalf("the viewer was repainted %d times in 10s", len(at))
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < termRepaintGap {
			t.Fatalf("repaint %d came %v after the one before, sooner than %v", i, d, termRepaintGap)
		}
	}
}
