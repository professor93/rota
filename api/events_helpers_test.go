package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
)

// The helpers every test of the server's events uses. They only read what a
// server published, or open its stream; nothing here changes a server.

// published is every event s still keeps, oldest first, of the given types,
// or of every type when none is given.
func published(s *Server, types ...string) []events.Event {
	evs, _, _ := s.events.Since(0)
	if len(types) == 0 {
		return evs
	}
	var out []events.Event
	for _, e := range evs {
		if slices.Contains(types, e.Type) {
			out = append(out, e)
		}
	}
	return out
}

// waitPublished waits up to within for s to have published an event of
// type typ, and returns the first one.
func waitPublished(t *testing.T, s *Server, typ string, within time.Duration) events.Event {
	t.Helper()
	end := time.Now().Add(within)
	for {
		if evs := published(s, typ); len(evs) > 0 {
			return evs[0]
		}
		if time.Now().After(end) {
			t.Fatalf("no %s was published within %v; what was: %v", typ, within, published(s))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// openEventStream serves h on a loopback server of its own and opens
// GET /v1/events?<query> on it with header, failing the test unless it is
// 200. The function it returns closes the stream and the server, and is also
// run when the test ends. (input_test.go has the run streams' openStream.)
func openEventStream(t *testing.T, h http.Handler, query string, header http.Header) (*events.Reader, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	rd, closeStream := streamFrom(t, srv.URL, query, header)
	done := func() {
		closeStream()
		srv.Close()
	}
	t.Cleanup(done)
	return rd, done
}

// streamFrom opens GET <base>/v1/events?<query> on a server already
// running, through a client of its own, failing the test unless it is 200.
// The function it returns closes the stream; closing twice is fine.
func streamFrom(t *testing.T, base, query string, header http.Header) (*events.Reader, func()) {
	t.Helper()
	resp := eventsGet(t, base, query, header)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/events?%s answered %d", query, resp.StatusCode)
	}
	closed := false
	return events.NewReader(resp.Body), func() {
		if !closed {
			closed = true
			resp.Body.Close()
		}
	}
}

// eventsGet is one GET /v1/events?<query> on base with header, through a
// transport of its own so that no connection is shared with another test.
// The caller closes the body.
func eventsGet(t *testing.T, base, query string, header http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/events?"+query, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// nextEvent is rd's next event, failing the test when none comes within d.
func nextEvent(t *testing.T, rd *events.Reader, d time.Duration) events.Event {
	t.Helper()
	e, err := nextOrErr(rd, d)
	if err != nil {
		t.Fatalf("reading the next event: %v", err)
	}
	return e
}

// errNoEvent is nextOrErr's answer when nothing came in time.
type errNoEvent time.Duration

func (e errNoEvent) Error() string { return "no event within " + time.Duration(e).String() }

// nextOrErr is rd's next event or the error it gave, or errNoEvent when
// neither came within d. The read goes on in the background after that, so
// a caller that got errNoEvent must not read rd again.
func nextOrErr(rd *events.Reader, d time.Duration) (events.Event, error) {
	type got struct {
		e   events.Event
		err error
	}
	ch := make(chan got, 1)
	go func() {
		e, err := rd.Next()
		ch <- got{e, err}
	}()
	select {
	case g := <-ch:
		return g.e, g.err
	case <-time.After(d):
		return events.Event{}, errNoEvent(d)
	}
}

// setFor sets *v to val for the rest of the test.
func setFor[T any](t *testing.T, v *T, val T) {
	t.Helper()
	old := *v
	*v = val
	t.Cleanup(func() { *v = old })
}

// eventsServer is a foreground server, one without HoldCommand, with a
// watch token beside the server's own.
func eventsServer(t *testing.T, tune ...func(*Options)) *harness {
	t.Helper()
	opts := Options{Tokens: []TokenPrincipal{{Name: "looker", Role: RoleWatch, SHA256: sumOf("watch-token-eeee")}}}
	for _, f := range tune {
		f(&opts)
	}
	return newHarness(t, opts)
}

func sumOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// bearer is the header a request with this token carries.
func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

// say publishes one event of type typ on s with text, and returns it.
func say(s *Server, typ, text string) events.Event {
	return s.emit(events.Event{Type: typ, Text: text})
}

// refusalOf is the error an answer's body carries.
func refusalOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var doc struct {
		Error string `json:"error"`
	}
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the answer is not an error document: %d %q", resp.StatusCode, raw)
	}
	return doc.Error
}
