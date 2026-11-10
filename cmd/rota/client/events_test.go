//go:build linux || darwin

package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
)

// eventsDaemon is a daemon whose GET /v1/events is serve: each request's
// query goes to the channel it returns first, so a test can see what was
// asked for.
func eventsDaemon(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) (*Client, chan string) {
	t.Helper()
	dir := terminals(t)
	queries := make(chan string, 8)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
			queries <- r.URL.RawQuery
			serve(w, r)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	return c, queries
}

// stream writes the head of a stream and then each of frames, flushing
// after each so the client sees them as they come.
func stream(w http.ResponseWriter, frames ...[]byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, events.Retry)
	for _, f := range frames {
		w.Write(f)
	}
	w.(http.Flusher).Flush()
}

func event(id int64, typ string) events.Event {
	return events.Event{ID: id, Type: typ, Time: time.UnixMicro(id).UTC(), Name: "work@x.com", Text: fmt.Sprintf("said %d", id)}
}

// The stream is read in order until the daemon ends it, which is io.EOF,
// and the query says where it starts and what it is for.
func TestEventsReadsInOrderToTheEnd(t *testing.T) {
	c, queries := eventsDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		stream(w, events.Frame(event(11, events.AccountDead)), []byte(events.Heartbeat),
			events.Frame(event(12, events.AccountReset)), events.Frame(event(13, events.AccountRotated)))
	})
	s, err := c.Events(context.Background(), 0, "account.*")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, want := range []int64{11, 12, 13} {
		e, err := s.Next()
		if err != nil || e.ID != want || e.Text != fmt.Sprintf("said %d", want) {
			t.Fatalf("Next: %+v %v, want event %d", e, err, want)
		}
	}
	if e, err := s.Next(); err != io.EOF {
		t.Fatalf("Next after the stream ended: %+v %v, want io.EOF", e, err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("Next again after the end: %v, want io.EOF", err)
	}
	if q := <-queries; q != "since=0&on=account.%2A" {
		t.Fatalf("the query was %q", q)
	}
	if s.Last() != 13 {
		t.Fatalf("Last after events 11 to 13 is %d", s.Last())
	}
}

// A negative since is from now: it is not sent, and nor is an empty on.
// Several patterns go as one on, joined by commas.
func TestEventsFromNowSendsNoSince(t *testing.T) {
	c, queries := eventsDaemon(t, func(w http.ResponseWriter, _ *http.Request) { stream(w) })
	for _, tc := range []struct {
		since int64
		on    []string
		want  string
	}{
		{-1, nil, ""},
		{-5, []string{"account.*", "session.ended"}, "on=account.%2A%2Csession.ended"},
		{42, nil, "since=42"},
	} {
		s, err := c.Events(context.Background(), tc.since, tc.on...)
		if err != nil {
			t.Fatal(err)
		}
		if q := <-queries; q != tc.want {
			t.Errorf("since %d on %v sent %q, want %q", tc.since, tc.on, q, tc.want)
		}
		// Nothing read yet: Last is where the stream was asked to start,
		// so asking again from it asks for the same.
		want := max(tc.since, -1)
		if s.Last() != want {
			t.Errorf("since %d: Last before any event is %d, want %d", tc.since, s.Last(), want)
		}
		s.Close()
	}
}

// A gap comes back as an event of its own, with the gap's words. Its id is
// 0 and it is not a place in the stream, so it does not move Last.
func TestAGapComesBackAsAnEvent(t *testing.T) {
	c, _ := eventsDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		stream(w, events.GapFrame(9), events.Frame(event(20, events.Test)))
	})
	s, err := c.Events(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.Next()
	if err != nil || e.Type != events.Gap || e.ID != 0 || e.Text != "events before 9 were not kept" {
		t.Fatalf("the first Next: %+v %v, want the gap", e, err)
	}
	if s.Last() != 3 {
		t.Fatalf("a gap moved Last to %d", s.Last())
	}
	if e, err := s.Next(); err != nil || e.ID != 20 || s.Last() != 20 {
		t.Fatalf("the event after the gap: %+v %v, Last %d", e, err, s.Last())
	}
}

// A frame that is not an event is skipped, and its id counts as read, so a
// reconnect does not ask for it again. Last only moves forward: an id below
// it, an id that is not a number, and a block with no data leave it alone.
func TestAFrameThatIsNotAnEventIsSkipped(t *testing.T) {
	c, _ := eventsDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		stream(w,
			[]byte("id: 15\nevent: account.dead\ndata: {not json\n\n"),
			events.Frame(event(20, events.AccountDead)),
			[]byte("id: 99\nevent: account.dead\n\n"), // no data: not a frame at all
			[]byte("id: x\ndata: {\"id\":1}\n\n"),     // no type, and an id that reads as 0
			[]byte("id: 17\ndata: {\"id\":17}\n\n"),   // below Last
			events.Frame(event(18, events.AccountReset)),
			[]byte("id: 40\ndata: [1,2]\n\n"),
		)
	})
	s, err := c.Events(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if e, err := s.Next(); err != nil || e.ID != 20 || s.Last() != 20 {
		t.Fatalf("past a bad frame: %+v %v, Last %d", e, err, s.Last())
	}
	if e, err := s.Next(); err != nil || e.ID != 18 || s.Last() != 20 {
		t.Fatalf("an event below Last: %+v %v, Last %d, want it given and Last still 20", e, err, s.Last())
	}
	if _, err := s.Next(); err != io.EOF || s.Last() != 40 {
		t.Fatalf("a bad frame at the end: %v, Last %d, want io.EOF and 40", err, s.Last())
	}
}

// Close makes a Next that is waiting return at once, with an error that is
// not the daemon ending the stream.
func TestCloseUnblocksNext(t *testing.T) {
	c, _ := eventsDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		stream(w, events.Frame(event(5, events.Test)))
		<-r.Context().Done()
	})
	s, err := c.Events(context.Background(), -1)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := s.Next(); err != nil || e.ID != 5 {
		t.Fatalf("Next: %+v %v", e, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	closed := time.Now()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatalf("Next after Close: %v, want an error that is not io.EOF", err)
		}
		if took := time.Since(closed); took > 100*time.Millisecond {
			t.Fatalf("Next returned %s after Close", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next still waits 2s after Close")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
	if _, err := s.Next(); err == nil || err == io.EOF {
		t.Fatalf("Next on a closed stream: %v", err)
	}

	// The caller's context ending is a Close of its own.
	ctx, cancel := context.WithCancel(context.Background())
	s, err = c.Events(ctx, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Next()
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := s.Next(); err == nil || err == io.EOF {
		t.Fatalf("Next after its context ended: %v", err)
	}
}

// An answer that is not 200 is a *Refusal in the daemon's words, or in
// rota's when it gave none.
func TestARefusedStreamIsARefusal(t *testing.T) {
	c, _ := eventsDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("on") == "nope" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"on: nope is not an event type"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := c.Events(context.Background(), 0, "nope")
	var r *Refusal
	if !errors.As(err, &r) || r.Status != 400 || r.Message != "on: nope is not an event type" {
		t.Fatalf("a 400: %#v", err)
	}
	_, err = c.Events(context.Background(), 0)
	if !errors.As(err, &r) || r.Status != 401 || r.Message != "the rota daemon answered 401 Unauthorized to GET /v1/events" {
		t.Fatalf("a 401 with no words: %#v", err)
	}
}

// TestEvent posts a test event for the sink it names, or for every sink,
// and says the id the daemon gave it.
func TestTestEventPostsAndSaysTheID(t *testing.T) {
	dir := terminals(t)
	bodies := make(chan string, 2)
	daemonAt(t, dir, twoSessions, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			bodies <- string(raw)
			if r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("Content-Type %q", r.Header.Get("Content-Type"))
			}
			w.WriteHeader(http.StatusAccepted)
			fmt.Fprint(w, `{"id":42}`)
		})
	})
	c, err := Dial(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := c.TestEvent("tg"); err != nil || id != 42 {
		t.Fatalf("TestEvent: %d %v", id, err)
	}
	if b := <-bodies; b != `{"type":"test","sink":"tg"}` {
		t.Fatalf("the body was %s", b)
	}
	if _, err := c.TestEvent(""); err != nil {
		t.Fatal(err)
	}
	if b := <-bodies; b != `{"type":"test"}` {
		t.Fatalf("a test for every sink sent %s", b)
	}
}
