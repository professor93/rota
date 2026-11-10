package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/professor93/rota/internal/events"
)

// maxRefusal bounds what is read of a stream's refusal: one sentence.
const maxRefusal = 64 << 10

// ErrStreamClosed is what Next gives once the stream was closed, by Close or
// by the end of the context it was opened with. It is not io.EOF, which is
// the daemon ending the stream.
var ErrStreamClosed = fmt.Errorf("the event stream was closed: %w", net.ErrClosed)

// EventStream is the daemon's events as GET /v1/events streams them, read
// one at a time with Next. One goroutine reads it; Close and Last may be
// called from any.
type EventStream struct {
	sock   string
	body   io.ReadCloser
	r      *events.Reader
	cancel context.CancelFunc
	ctx    context.Context

	last   atomic.Int64
	closed sync.Once
}

// Events opens the daemon's event stream. since is where it starts: below 0
// is from now, 0 is every event the daemon still keeps, and an id is every
// kept event after it, with a gap first when some after it were not kept.
// on is the event types to read, each a type, "<prefix>.*" or "*"; none is
// every type. The stream lasts as long as ctx does, with no timeout of its
// own. An answer that is not 200, a pattern the daemon does not know among
// them, is a *Refusal; a 401 or 403 is the stream's sign-in no longer
// holding, which asking again will not mend.
func (c *Client) Events(ctx context.Context, since int64, on ...string) (*EventStream, error) {
	var q []string
	if since >= 0 {
		q = append(q, "since="+strconv.FormatInt(since, 10))
	}
	if len(on) > 0 {
		q = append(q, "on="+url.QueryEscape(strings.Join(on, ",")))
	}
	path := "/v1/events"
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	ctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://rota"+path, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("the rota daemon on %s: %w", c.sock, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefusal))
		return nil, refusal(resp, raw, http.MethodGet, "/v1/events")
	}
	s := &EventStream{sock: c.sock, body: resp.Body, r: events.NewReader(resp.Body), cancel: cancel, ctx: ctx}
	s.last.Store(max(since, -1))
	return s, nil
}

// Next is the stream's next event, in the order the daemon published them.
//
// A gap, events the daemon no longer kept, comes back as an Event whose Type
// is events.Gap and whose ID is 0. A frame that is not an event, from a
// daemon of another version say, is skipped, and counts as read for Last.
//
// The daemon ending the stream, as it does when it stops, is io.EOF. Once
// the stream is closed, Next gives ErrStreamClosed. Any other error is the
// stream broken. After an error every later call gives an error too.
func (s *EventStream) Next() (events.Event, error) {
	for {
		if s.ctx.Err() != nil {
			return events.Event{}, ErrStreamClosed
		}
		e, err := s.r.Next()
		var fe *events.FrameError
		switch {
		case errors.As(err, &fe):
			s.saw(fe.ID)
			continue
		case err == io.EOF:
			if s.ctx.Err() != nil {
				return events.Event{}, ErrStreamClosed
			}
			return events.Event{}, io.EOF
		case err != nil:
			if s.ctx.Err() != nil {
				return events.Event{}, ErrStreamClosed
			}
			return events.Event{}, fmt.Errorf("the rota daemon on %s: %w", s.sock, err)
		}
		if e.Type != events.Gap {
			s.saw(e.ID)
		}
		return e, nil
	}
}

// Last is the highest id the stream has read, the one to open it again from
// so that nothing read is read twice. Before any event it is the since the
// stream was opened with, -1 for one opened from now: Events(ctx, s.Last())
// always asks for what comes after what this stream asked for or read. A gap
// never moves it, and it never goes back.
func (s *EventStream) Last() int64 { return s.last.Load() }

// saw moves Last forward to id, and never back.
func (s *EventStream) saw(id int64) {
	for {
		old := s.last.Load()
		if id <= old || s.last.CompareAndSwap(old, id) {
			return
		}
	}
}

// Close ends the stream: a Next that is waiting returns ErrStreamClosed at
// once, and so does every later one. Closing twice is fine.
func (s *EventStream) Close() error {
	s.closed.Do(func() {
		// The context first: it closes the connection, which a read in
		// progress holds the body's lock against.
		s.cancel()
		s.body.Close()
	})
	return nil
}

// TestEvent publishes a test event for the sink named, or for every sink
// when sink is "", and says the event's id. A sink gets it whatever its on
// says.
func (c *Client) TestEvent(sink string) (int64, error) {
	in := struct {
		Type string `json:"type"`
		Sink string `json:"sink,omitempty"`
	}{events.Test, sink}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := c.do(http.MethodPost, "/v1/events", in, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// refusal is an answer that is not a success as a *Refusal: in the daemon's
// words when its body has them, else in rota's.
func refusal(resp *http.Response, raw []byte, method, path string) *Refusal {
	var e struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
		Holder string `json:"holder"`
	}
	if json.Unmarshal(raw, &e) != nil || strings.TrimSpace(e.Error) == "" {
		e.Error = fmt.Sprintf("the rota daemon answered %s to %s %s", resp.Status, method, path)
	}
	return &Refusal{Status: resp.StatusCode, Reason: e.Reason, Message: e.Error, Holder: e.Holder}
}
