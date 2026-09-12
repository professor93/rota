package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/professor93/rota/internal/fakecli"
)

// echoClaude puts a claude on PATH that answers every message for as long as
// its stdin stays open, which is the whole point of a run that stays open.
func echoClaude(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	fakecli.Install(t, bin, "claude", fakecli.Spec{Echo: true})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// stream is one streaming response, read line by line on a goroutine.
//
// A test about an open run has to go on making requests while the run is
// happening — that is what "open" means — so the reading cannot be the
// io.ReadAll every other test here does.
type stream struct {
	cancel context.CancelFunc
	resp   *http.Response
	lines  chan string

	mu   sync.Mutex
	text strings.Builder
}

func openStream(t *testing.T, h *harness, method, path string, body any, hdr ...string) *stream {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, method, h.srv.URL+path, r)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	s := &stream{cancel: cancel, resp: resp, lines: make(chan string, 4096)}
	go func() {
		defer close(s.lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			line := sc.Text()
			s.mu.Lock()
			s.text.WriteString(line + "\n")
			s.mu.Unlock()
			s.lines <- line
		}
	}()
	t.Cleanup(func() { cancel(); resp.Body.Close() })
	return s
}

// all is everything read so far, for an error message that has to say what
// did arrive.
func (s *stream) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// line waits for the next line, or says what the stream did instead.
func (s *stream) line(t *testing.T) (string, bool) {
	t.Helper()
	select {
	case l, ok := <-s.lines:
		return l, ok
	case <-time.After(15 * time.Second):
		t.Fatalf("nothing more arrived on the stream; so far:\n%s", s.all())
		return "", false
	}
}

// ev waits for the next event on an NDJSON stream.
func (s *stream) ev(t *testing.T) map[string]any {
	t.Helper()
	for {
		l, ok := s.line(t)
		if !ok {
			t.Fatalf("the stream ended before the event came; so far:\n%s", s.all())
		}
		if doc := asEvent(l); doc != nil {
			return doc
		}
	}
}

// rest reads what is left until the stream closes.
func (s *stream) rest(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for {
		l, ok := s.line(t)
		if !ok {
			return out
		}
		if doc := asEvent(l); doc != nil {
			out = append(out, doc)
		}
	}
}

// waitFor reads until an event of this type arrives, and returns it with
// everything that came before it.
func (s *stream) waitFor(t *testing.T, kind string) (map[string]any, []map[string]any) {
	t.Helper()
	var seen []map[string]any
	for {
		ev := s.ev(t)
		if ev["type"] == kind {
			return ev, seen
		}
		seen = append(seen, ev)
	}
}

// asEvent reads one line of a stream as an event: NDJSON as it is, SSE from
// its data line, and nothing at all for the framing around them.
func asEvent(line string) map[string]any {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data: "))
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var doc map[string]any
	if json.Unmarshal([]byte(line), &doc) != nil {
		return nil
	}
	return doc
}

// post is one request to a run's endpoints: the code, and the document.
func post(t *testing.T, h *harness, method, path string, body any) (int, map[string]any) {
	t.Helper()
	resp, raw := h.do(method, path, body)
	var doc map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode, doc
}

// startInputRun opens a run that stays open and returns its stream and id.
func startInputRun(t *testing.T, h *harness, prompt string) (*stream, string) {
	t.Helper()
	s := openStream(t, h, "POST", "/v1/accounts/1/run",
		map[string]any{"prompt": prompt, "stream": true, "input": true}, "Accept", "application/x-ndjson")
	init := s.ev(t)
	if init["type"] != "init" {
		t.Fatalf("a stream opens with rota's own event: %v", init)
	}
	id, _ := init["run_id"].(string)
	if id == "" {
		t.Fatalf("an open run is addressable, so its opening event says its id: %v", init)
	}
	// Nothing is left running after the test, whatever it proved.
	t.Cleanup(func() { h.do("POST", "/v1/runs/"+id+"/close", nil) })
	return s, id
}

// A run started with "input" goes on taking messages over HTTP: each one is
// answered in the same conversation, an interrupt is acknowledged, a steer
// starts the next turn, and the stream says what became of every one of them.
func TestAnInputRunTakesMessagesOverHTTP(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")

	code, doc := post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "two"})
	two, _ := doc["id"].(string)
	if code != http.StatusAccepted || two == "" || doc["state"] != "accepted" {
		t.Fatalf("a message reaches a running run: %d %v", code, doc)
	}
	code, doc = post(t, h, "POST", "/v1/runs/"+id+"/interrupt", nil)
	if code != http.StatusAccepted || doc["id"] == "" {
		t.Fatalf("an interrupt is acknowledged by id: %d %v", code, doc)
	}
	code, doc = post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "three", "steer": true})
	three, _ := doc["id"].(string)
	if code != http.StatusAccepted || three == "" {
		t.Fatalf("a steer is a message too: %d %v", code, doc)
	}
	code, doc = post(t, h, "POST", "/v1/runs/"+id+"/close", nil)
	if code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("close says there is nothing more: %d %v", code, doc)
	}

	events := s.rest(t)
	var texts []string
	accepted, answered := map[string]bool{}, map[string]bool{}
	interrupted, idle := 0, 0
	for _, ev := range events {
		evID, _ := ev["id"].(string)
		switch ev["type"] {
		case "text":
			text, _ := ev["text"].(string)
			texts = append(texts, text)
		case "input":
			switch ev["state"] {
			case "accepted":
				accepted[evID] = true
			case "answered":
				answered[evID] = true
			default:
				t.Fatalf("no message failed: %v", ev)
			}
		case "interrupted":
			interrupted++
		case "idle":
			idle++
		}
		if ev["type"] != "done" && ev["seq"] == nil {
			t.Fatalf("every event in a stream carries its place in it: %v", ev)
		}
	}
	for _, want := range []string{"echo: one", "echo: two", "echo: three"} {
		if !hasText(texts, want) {
			t.Fatalf("every message is answered in the same run: %q", texts)
		}
	}
	if !accepted[two] || !accepted[three] || !answered[two] || !answered[three] {
		t.Fatalf("both messages are accepted and then answered: %v %v", accepted, answered)
	}
	if interrupted < 2 {
		t.Fatalf("the interrupt and the steer are each acknowledged: %d", interrupted)
	}
	if idle == 0 {
		t.Fatal("a run with nothing left waiting says so")
	}
	last := events[len(events)-1]
	if last["type"] != "done" {
		t.Fatalf("the stream ends by saying how the run ended: %v", last)
	}
}

func hasText(texts []string, want string) bool {
	for _, got := range texts {
		if strings.Contains(got, want) {
			return true
		}
	}
	return false
}

// The connection is not the run. A reader that drops is given the grace
// period to come back, and what it missed while it was away is replayed to it
// when it does.
func TestARunSurvivesADroppedReaderAndReplaysOnReattach(t *testing.T) {
	h := newHarness(t, Options{InputGrace: 5 * time.Second})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")
	init := 1 // the opening event is the first of the stream
	s.cancel()

	code, doc := post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "two"})
	two, _ := doc["id"].(string)
	if code != http.StatusAccepted || two == "" {
		t.Fatalf("the run is alive with nobody reading it: %d %v", code, doc)
	}

	back := openStream(t, h, "GET", "/v1/runs/"+id+"/events?since="+strconv.Itoa(init), nil,
		"Accept", "application/x-ndjson")
	code, _ = post(t, h, "POST", "/v1/runs/"+id+"/close", nil)
	if code != http.StatusOK {
		t.Fatalf("close: %d", code)
	}
	events := back.rest(t)

	var texts []string
	accepted, answered := false, false
	for _, ev := range events {
		if ev["seq"] != nil && int(ev["seq"].(float64)) <= init {
			t.Fatalf("since asks for what came after it: %v", ev)
		}
		if text, ok := ev["text"].(string); ok {
			texts = append(texts, text)
		}
		if ev["type"] == "input" && ev["id"] == two {
			accepted = accepted || ev["state"] == "accepted"
			answered = answered || ev["state"] == "answered"
		}
	}
	if !hasText(texts, "echo: one") {
		t.Fatalf("what the reader missed is replayed to it: %q\n%s", texts, back.all())
	}
	if !hasText(texts, "echo: two") || !accepted || !answered {
		t.Fatalf("and the run goes on from there: %q accepted %v answered %v", texts, accepted, answered)
	}
	if last := events[len(events)-1]; last["type"] != "done" {
		t.Fatalf("a reattached stream ends the same way: %v", last)
	}
}

// The grace is a grace, not a lease: a run nobody comes back to is stopped
// and closed, so the account it was spending is let go.
func TestAnUnattendedRunIsClosedAfterTheGrace(t *testing.T) {
	h := newHarness(t, Options{InputGrace: 200 * time.Millisecond})
	echoClaude(t)
	s, id := startInputRun(t, h, "one")
	s.cancel()
	// The grace is short, and what follows it is a CLI being stopped and
	// waited for: the deadline is generous, the fact is not.
	var code int
	var doc map[string]any
	for deadline := time.Now().Add(10 * time.Second); ; {
		code, doc = post(t, h, "GET", "/v1/runs/"+id, nil)
		if code != http.StatusOK || doc["state"] == "ended" || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code != http.StatusOK || doc["state"] != "ended" {
		t.Fatalf("a run nobody read is over: %d %v", code, doc)
	}
	code, doc = post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "two"})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(doc["error"]), "has ended") {
		t.Fatalf("and says so to whoever sends into it: %d %v", code, doc)
	}
}

// A run can sit idle for as long as the person talking to it is thinking. The
// heartbeat is what keeps the proxy between them from deciding otherwise.
func TestHeartbeatsKeepAnIdleStreamAlive(t *testing.T) {
	was := heartbeat
	heartbeat = 100 * time.Millisecond
	t.Cleanup(func() { heartbeat = was })
	h := newHarness(t, Options{InputGrace: 100 * time.Millisecond})
	echoClaude(t)

	sse := openStream(t, h, "POST", "/v1/accounts/1/run",
		map[string]any{"prompt": "one", "stream": true, "input": true})
	var id string
	deadline := time.Now().Add(5 * time.Second)
	for id == "" || !strings.Contains(sse.all(), ": ping") {
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat on an SSE stream:\n%s", sse.all())
		}
		line, ok := sse.line(t)
		if !ok {
			t.Fatalf("the stream ended:\n%s", sse.all())
		}
		if ev := asEvent(line); ev != nil && ev["type"] == "init" {
			id, _ = ev["run_id"].(string)
		}
	}
	post(t, h, "POST", "/v1/runs/"+id+"/close", nil)

	nd, id2 := startInputRun(t, h, "one")
	var ping map[string]any
	for ping == nil {
		line, ok := nd.line(t)
		if !ok {
			t.Fatalf("the stream ended before a heartbeat:\n%s", nd.all())
		}
		if ev := asEvent(line); ev != nil && ev["type"] == "ping" {
			ping = ev
		}
	}
	if ping["seq"] != nil {
		t.Fatalf("a heartbeat is not an event of the run, so it is not numbered: %v", ping)
	}
	post(t, h, "POST", "/v1/runs/"+id2+"/close", nil)
}

// The endpoints say what they cannot do, and say it by id: an id nobody knows
// is a 404, a run that has ended is a 409, and a message with no text is a
// request nobody could act on.
func TestRunEndpointsRefuseWhatTheyCannot(t *testing.T) {
	h := newHarness(t, Options{})
	echoClaude(t)
	for _, call := range [][2]string{
		{"POST", "/v1/runs/nosuchrun/messages"}, {"POST", "/v1/runs/nosuchrun/interrupt"},
		{"POST", "/v1/runs/nosuchrun/close"}, {"GET", "/v1/runs/nosuchrun/events"},
		{"GET", "/v1/runs/nosuchrun"},
	} {
		code, doc := post(t, h, call[0], call[1], nil)
		if code != http.StatusNotFound || !strings.Contains(fmt.Sprint(doc["error"]), "no run nosuchrun") {
			t.Fatalf("%s %s: %d %v", call[0], call[1], code, doc)
		}
	}

	s, id := startInputRun(t, h, "one")
	code, doc := post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": ""})
	if code != http.StatusBadRequest {
		t.Fatalf("a message with no text is nothing to send: %d %v", code, doc)
	}
	code, doc = post(t, h, "GET", "/v1/runs", nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(doc["runs"]), id) {
		t.Fatalf("a live run is listed: %d %v", code, doc)
	}

	post(t, h, "POST", "/v1/runs/"+id+"/close", nil)
	if last, _ := s.waitFor(t, "done"); last["type"] != "done" {
		t.Fatal("the run must end after close")
	}
	code, doc = post(t, h, "POST", "/v1/runs/"+id+"/messages", map[string]any{"text": "more"})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(doc["error"]), "has ended") {
		t.Fatalf("nothing is sent into a run that has ended: %d %v", code, doc)
	}
	// Closing is idempotent: a caller that says it twice meant it once.
	if code, doc = post(t, h, "POST", "/v1/runs/"+id+"/close", nil); code != http.StatusOK || doc["ok"] != true {
		t.Fatalf("close again: %d %v", code, doc)
	}
	code, doc = post(t, h, "GET", "/v1/runs", nil)
	if code != http.StatusOK || !strings.Contains(fmt.Sprint(doc["runs"]), id) {
		t.Fatalf("a run that has just ended is still listed: %d %v", code, doc)
	}
}

// Everything about a run that did not ask to stay open is exactly as it was.
func TestAnOrdinaryRunIsUntouched(t *testing.T) {
	h := newHarness(t, Options{})
	resp, raw := h.do("POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "stream": true},
		"Accept", "application/x-ndjson")
	body := string(raw)
	if resp.StatusCode != 200 || !strings.Contains(body, `"type":"init"`) || !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "run_id") || strings.Contains(body, "ping") {
		t.Fatalf("a run nobody can send into has no id to send to, and no heartbeat: %s", body)
	}
	code, doc := post(t, h, "GET", "/v1/runs", nil)
	if code != http.StatusOK || fmt.Sprint(doc["runs"]) != "[]" {
		t.Fatalf("and it is not one of the runs that stay open: %d %v", code, doc)
	}
}

// input is a stream, and the refusal names the field to add rather than
// leaving the caller to find out from a CLI's own complaint.
func TestInputNeedsAStream(t *testing.T) {
	h := newHarness(t, Options{})
	code, doc := post(t, h, "POST", "/v1/accounts/1/run", map[string]any{"prompt": "p", "input": true})
	if code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(doc["error"]), "stream") {
		t.Fatalf("%d %v", code, doc)
	}
}
