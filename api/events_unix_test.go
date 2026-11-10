//go:build linux || darwin

package api

import (
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// The server's events that need a unix machine: the daemon's own, which
// need a holder, and a reader that stops reading, which needs a socket's
// buffers made small.

/* ---------------------------------------------------------- the daemon --- */

// A daemon says it started, with what it took up, after it has taken it up;
// and says it is stopping as its shutdown begins, as the last event its
// ring takes.
func TestADaemonSaysItStartedAndIsStopping(t *testing.T) {
	p := withHeld(t, nil)
	if got := published(p.api, events.DaemonStarted); len(got) != 1 ||
		got[0].Text != fmt.Sprintf("The rota daemon %s started for %s with 0 sessions running and 0 stopped", wire.Version, p.dir) {
		t.Fatalf("an empty daemon published %v", got)
	}
	id := p.start(nil)["id"].(string)
	writeRecord(t, p.terminals(), holdwire.Meta{Exit: code(0), Ended: time.Now().Add(-time.Minute)})

	q := p.restart()
	want := fmt.Sprintf("The rota daemon %s started for %s with 1 session running and 1 stopped", wire.Version, q.dir)
	got := published(q.api, events.DaemonStarted)
	if len(got) != 1 || got[0].Text != want || got[0].Name != "" || got[0].Account != 0 || got[0].Session != "" {
		t.Fatalf("the daemon that took %s up published %+v, want %q", id, got, want)
	}

	q.api.StopRefreshing()
	all := published(q.api)
	last := all[len(all)-1]
	if last.Type != events.DaemonStopped || last.Text != fmt.Sprintf("The rota daemon for %s is stopping; its 1 running session goes on", q.dir) {
		t.Fatalf("the last event is %+v", last)
	}
	if _, ok := q.api.events.Publish(events.Event{Type: events.Test}); ok {
		t.Fatal("the ring took an event after daemon.stopped")
	}
	q.api.StopRefreshing() // once is enough, and twice says nothing more
	if n := len(published(q.api, events.DaemonStopped)); n != 1 {
		t.Fatalf("daemon.stopped was published %d times", n)
	}
}

// A browser that reconnects to the next daemon with the id it last read is
// given that daemon's events from its start, and nothing twice: ids keep
// rising across a restart (Review Focus 3).
func TestLastEventIDSurvivesARestart(t *testing.T) {
	p := withHeld(t, nil)
	first := waitPublished(t, p.api, events.DaemonStarted, time.Second)
	q := p.restart()
	hdr := bearer(q.token)
	hdr.Set("Last-Event-ID", strconv.FormatInt(first.ID, 10))
	rd, _ := openEventStream(t, q.api.Handler(), "", hdr)
	e := nextEvent(t, rd, 2*time.Second)
	if e.Type != events.DaemonStarted || e.ID <= first.ID || !strings.Contains(e.Text, "started for "+q.dir) {
		t.Fatalf("after a restart the stream gave %+v first, want the new daemon's daemon.started", e)
	}
	marker := say(q.api, events.SessionEnded, "marker")
	if e := nextEvent(t, rd, 2*time.Second); e.ID != marker.ID {
		t.Fatalf("after daemon.started the stream gave %+v, want the marker", e)
	}
}

/* ---------------------------------------------------- a stuck reader --- */

// smallBuffers is a listener whose connections send through a small buffer,
// so that a reader that stops reading is felt at once rather than after the
// megabytes Linux's loopback would otherwise take.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4 << 10)
	}
	return c, err
}

// A stream whose reader has stopped reading costs the server nothing: once
// its socket is full, a write that does not finish within eventWrite ends
// it, and nobody publishing ever waits on it (Review Focus 3).
//
// A publish that waited on the stream would wait about eventWrite, once, so
// eventWrite is long beside what a loaded machine under -race can pause a
// goroutine for, and the bound sits halfway to it.
func TestAStuckReaderIsLetGo(t *testing.T) {
	setFor(t, &eventWrite, time.Second)
	h := eventsServer(t)
	srv := httptest.NewUnstartedServer(h.api.Handler())
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()

	// A client whose receive buffer is as small as the kernel allows, set
	// before it connects so the window it offers is small from the start.
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<10)
		}); err != nil {
			return err
		}
		return serr
	}}
	conn, err := d.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /v1/events HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", h.token)
	for end := time.Now().Add(2 * time.Second); h.api.streams.Load() != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the stream never opened")
		}
	}

	text := strings.Repeat("x", 1000)
	var slowest time.Duration
	n := 0
	for ; n < 20000 && h.api.streams.Load() > 0; n++ {
		began := time.Now()
		say(h.api, events.SessionEnded, text)
		slowest = max(slowest, time.Since(began))
	}
	cut := time.Now().Add(2 * time.Second)
	for h.api.streams.Load() > 0 {
		if time.Now().After(cut) {
			t.Fatalf("after %d events the stuck stream is still open", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if slowest > eventWrite/2 {
		t.Errorf("publishing waited %v once, with a stuck reader", slowest)
	}
	// And the server has hung up: what the socket held is read, and then
	// the end of it.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil && !isReset(err) {
		t.Fatalf("the server did not hang up on the stuck reader: %v", err)
	}
}

// isReset is a connection the far end closed with data still unread, which
// is a hang-up as much as an EOF is.
func isReset(err error) bool {
	return strings.Contains(err.Error(), "connection reset")
}
