package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
)

// These are about the server's own events: the ring every server keeps, the
// stream GET /v1/events reads it through, and the test event POST takes.
// The daemon's own events, which need a holder, are events_unix_test.go's.

/* ------------------------------------------------------- where it starts --- */

// since=0 is everything kept, since=<id> is what came after it, and neither
// is from now: only what is published after the stream opened.
func TestAStreamStartsWhereItIsAsked(t *testing.T) {
	h := eventsServer(t)
	one := say(h.api, events.SessionEnded, "one")
	say(h.api, events.SessionEnded, "two")
	say(h.api, events.SessionEnded, "three")

	all, _ := openEventStream(t, h.api.Handler(), "since=0", bearer(h.token))
	var last int64
	for i, want := range []string{"one", "two", "three"} {
		e := nextEvent(t, all, 2*time.Second)
		if e.Text != want || e.ID <= last {
			t.Fatalf("event %d is %+v, want %q with an id after %d", i, e, want, last)
		}
		last = e.ID
	}

	after, _ := openEventStream(t, h.api.Handler(), "since="+strconv.FormatInt(one.ID, 10), bearer(h.token))
	for _, want := range []string{"two", "three"} {
		if e := nextEvent(t, after, 2*time.Second); e.Text != want {
			t.Fatalf("after the first, %+v came, want %q", e, want)
		}
	}

	now, _ := openEventStream(t, h.api.Handler(), "", bearer(h.token))
	say(h.api, events.SessionEnded, "four")
	if e := nextEvent(t, now, 2*time.Second); e.Text != "four" {
		t.Fatalf("a stream from now was given %+v first, want four", e)
	}

	resp := eventsGet(t, h.srv.URL, "since=yesterday", bearer(h.token))
	if resp.StatusCode != http.StatusBadRequest || refusalOf(t, resp) != "since must be an event id" {
		t.Fatalf("since=yesterday answered %d", resp.StatusCode)
	}
}

// Last-Event-ID is where a browser's EventSource says it got to, and it
// beats since.
func TestLastEventIDBeatsSince(t *testing.T) {
	h := eventsServer(t)
	one := say(h.api, events.SessionEnded, "one")
	say(h.api, events.SessionEnded, "two")
	say(h.api, events.SessionEnded, "three")
	hdr := bearer(h.token)
	hdr.Set("Last-Event-ID", strconv.FormatInt(one.ID, 10))
	rd, _ := openEventStream(t, h.api.Handler(), "since=0", hdr)
	for _, want := range []string{"two", "three"} {
		if e := nextEvent(t, rd, 2*time.Second); e.Text != want {
			t.Fatalf("after Last-Event-ID, %+v came, want %q", e, want)
		}
	}
}

// on keeps the events a stream asks for, and refuses a pattern that could
// never match, in the contract's words.
func TestOnFilters(t *testing.T) {
	h := eventsServer(t)
	say(h.api, events.AccountThreshold, "a")
	say(h.api, events.SessionEnded, "s")
	say(h.api, events.AccountDead, "b")
	say(h.api, events.ViewerJoined, "v")
	marker := say(h.api, events.AccountReset, "c")

	rd, _ := openEventStream(t, h.api.Handler(), "since=0&on=account.*", bearer(h.token))
	for _, want := range []string{"a", "b", "c"} {
		if e := nextEvent(t, rd, 2*time.Second); e.Text != want || !strings.HasPrefix(e.Type, "account.") {
			t.Fatalf("on=account.* gave %+v, want %q", e, want)
		}
	}
	// A stream that skipped events past what it matched has moved past
	// them, so the next one it is given is what comes after.
	say(h.api, events.SessionEnded, "s2")
	say(h.api, events.AccountDead, "d")
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "d" || e.ID <= marker.ID {
		t.Fatalf("after the marker, %+v came, want d", e)
	}

	both, _ := openEventStream(t, h.api.Handler(), "since=0&on=viewer.joined,session.ended", bearer(h.token))
	for _, want := range []string{"s", "v", "s2"} {
		if e := nextEvent(t, both, 2*time.Second); e.Text != want {
			t.Fatalf("on=viewer.joined,session.ended gave %+v, want %q", e, want)
		}
	}

	resp := eventsGet(t, h.srv.URL, "on=session.atention", bearer(h.token))
	want := events.CheckPatterns([]string{"session.atention"}).Error()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("on=session.atention answered %d", resp.StatusCode)
	}
	if got := refusalOf(t, resp); got != want {
		t.Fatalf("on=session.atention is refused with %q, want %q", got, want)
	}
}

// A stream asked for events older than the ring keeps is told so, once and
// first, and then given what is kept. The gap is no place in the stream: it
// has no id.
func TestAGapIsSaid(t *testing.T) {
	setFor(t, &newEventRing, func(int, func() time.Time) *events.Ring { return events.NewRing(2, nil) })
	h := eventsServer(t)
	one := say(h.api, events.SessionEnded, "one")
	say(h.api, events.SessionEnded, "two")
	say(h.api, events.SessionEnded, "three")
	say(h.api, events.SessionEnded, "four")

	rd, _ := openEventStream(t, h.api.Handler(), "since="+strconv.FormatInt(one.ID, 10), bearer(h.token))
	if e := nextEvent(t, rd, 2*time.Second); e.Type != events.Gap || e.ID != 0 {
		t.Fatalf("a stale since was given %+v first, want a gap with no id", e)
	}
	for _, want := range []string{"three", "four"} {
		if e := nextEvent(t, rd, 2*time.Second); e.Text != want {
			t.Fatalf("after the gap, %+v came, want %q", e, want)
		}
	}

	// An id this ring never gave, past its newest, is not trusted: all of
	// it, after a gap.
	future, _ := openEventStream(t, h.api.Handler(), "since="+strconv.FormatInt(h.api.events.Last()+1e9, 10), bearer(h.token))
	if e := nextEvent(t, future, 2*time.Second); e.Type != events.Gap {
		t.Fatalf("an id from the future was given %+v first, want a gap", e)
	}
	if e := nextEvent(t, future, 2*time.Second); e.Text != "three" {
		t.Fatalf("after that gap, %+v came, want three", e)
	}
}

/* -------------------------------------------------------- a stream's life --- */

// A stream is a request that lasts, so the server's ReadTimeout, which is
// for reading a request, must not end it (Review Focus 3).
func TestAStreamOutlivesReadTimeout(t *testing.T) {
	h := eventsServer(t)
	srv := httptest.NewUnstartedServer(h.api.Handler())
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()
	rd, done := streamFrom(t, srv.URL, "", bearer(h.token))
	defer done()
	go func() {
		time.Sleep(800 * time.Millisecond)
		say(h.api, events.SessionEnded, "late")
	}()
	if e := nextEvent(t, rd, 3*time.Second); e.Text != "late" {
		t.Fatalf("the stream gave %+v, want the event published at 800ms", e)
	}
	time.Sleep(300 * time.Millisecond) // past a second in all
	say(h.api, events.SessionEnded, "later")
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "later" {
		t.Fatalf("the stream gave %+v after a second, want later", e)
	}
}

// Once shutdown begins every stream is ended, after the events it has not
// yet been given, so the server's own Close is not kept waiting on one
// (Review Focus 3).
func TestStreamsEndWhenShutdownBegins(t *testing.T) {
	h := eventsServer(t)
	rd, done := openEventStream(t, h.api.Handler(), "", bearer(h.token))
	say(h.api, events.SessionEnded, "before")
	h.api.StopRefreshing()
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "before" {
		t.Fatalf("the stream gave %+v, want the event published before shutdown", e)
	}
	if _, err := nextOrErr(rd, 2*time.Second); err != io.EOF {
		t.Fatalf("after StopRefreshing the stream gave %v, want EOF", err)
	}
	began := time.Now()
	done()
	if took := time.Since(began); took > time.Second {
		t.Fatalf("closing the server took %v with a stream that had ended", took)
	}
	// And a stream opened after that has nothing to wait for.
	late, _ := openEventStream(t, h.api.Handler(), "since=0", bearer(h.token))
	if e := nextEvent(t, late, 2*time.Second); e.Text != "before" {
		t.Fatalf("a stream after shutdown gave %+v, want what was kept", e)
	}
	if _, err := nextOrErr(late, 2*time.Second); err != io.EOF {
		t.Fatalf("a stream after shutdown gave %v, want EOF", err)
	}
	if e := say(h.api, events.SessionEnded, "after"); e.ID != 0 {
		t.Fatalf("an event was published after shutdown began: %+v", e)
	}
}

// A stream that says nothing for a while still sends a heartbeat, so a
// proxy or a phone does not take it for dead. Reader drops those, so this
// reads the body as it comes.
func TestHeartbeatsKeepAStreamAlive(t *testing.T) {
	setFor(t, &eventBeat, 50*time.Millisecond)
	h := eventsServer(t)
	resp := eventsGet(t, h.srv.URL, "", bearer(h.token))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the stream answered %d", resp.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream; charset=utf-8",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("%s is %q, want %q", k, got, want)
		}
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	want := []string{"retry: 3000", "", ": ping", "", ": ping"}
	timeout := time.After(3 * time.Second)
	for i, w := range want {
		select {
		case got, ok := <-lines:
			if !ok || got != w {
				t.Fatalf("line %d of the stream is %q, want %q", i, got, w)
			}
		case <-timeout:
			t.Fatalf("only %d lines of the stream came in time", i)
		}
	}
}

// A page sign-in that has signed out or expired, a person's or an invite's,
// ends its stream at the next heartbeat, so nobody goes on being told what
// they may no longer see (Review Focus 3). A bearer token's stream, which
// has no sign-in, goes on.
func TestAnExpiredSignInEndsItsStream(t *testing.T) {
	setFor(t, &eventBeat, 50*time.Millisecond)
	h := eventsServer(t)
	user, err := h.api.sessions.start(Principal{Name: "alice", Role: RoleWatch, Via: "user"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	invite, err := h.api.sessions.start(Principal{Name: "invite-abcdef", Role: RoleWatch, Via: "invite"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cookie := func(p *Principal) http.Header {
		return http.Header{"Cookie": {sessionCookie + "=" + p.session}}
	}
	alice, _ := openEventStream(t, h.api.Handler(), "", cookie(user))
	guest, _ := openEventStream(t, h.api.Handler(), "", cookie(invite))
	tok, _ := openEventStream(t, h.api.Handler(), "", bearer(h.token))
	say(h.api, events.SessionEnded, "seen")
	for _, rd := range []*events.Reader{alice, guest, tok} {
		if e := nextEvent(t, rd, 2*time.Second); e.Text != "seen" {
			t.Fatalf("a signed-in stream gave %+v, want seen", e)
		}
	}

	h.api.sessions.end(user.session)
	if _, err := nextOrErr(alice, 2*time.Second); err != io.EOF {
		t.Fatalf("a signed-out user's stream gave %v, want EOF", err)
	}
	// The invite's sign-in runs out, as the table's clock has it.
	h.api.sessions.mu.Lock()
	h.api.sessions.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	h.api.sessions.mu.Unlock()
	if _, err := nextOrErr(guest, 2*time.Second); err != io.EOF {
		t.Fatalf("an expired invite's stream gave %v, want EOF", err)
	}
	time.Sleep(150 * time.Millisecond) // a few heartbeats
	say(h.api, events.SessionEnded, "still")
	if e := nextEvent(t, tok, 2*time.Second); e.Text != "still" {
		t.Fatalf("a token's stream gave %+v, want still", e)
	}
}

// A server holds at most eventStreams streams at once from the network, and
// says why it refuses one more. The control socket's own, which the
// daemon's sinks read, is never one of them (I6).
func TestStreamsAreBounded(t *testing.T) {
	h := eventsServer(t)
	for range eventStreams {
		_, done := streamFrom(t, h.srv.URL, "", bearer(h.token))
		defer done() // before the harness's server, whose Close waits on them
	}
	resp := eventsGet(t, h.srv.URL, "", bearer(h.token))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("stream %d answered %d", eventStreams+1, resp.StatusCode)
	}
	if got := refusalOf(t, resp); got != "too many event streams are open" {
		t.Fatalf("stream %d is refused with %q", eventStreams+1, got)
	}
	local := h.api.LocalHandler(&Principal{Name: localName, Role: RoleControl, Via: "local"})
	rd, _ := openEventStream(t, local, "", nil)
	say(h.api, events.SessionEnded, "local")
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "local" {
		t.Fatalf("the local stream gave %+v", e)
	}
	if n := h.api.streams.Load(); n != eventStreams {
		t.Fatalf("streams counts %d, want %d: the local one is not counted", n, eventStreams)
	}
}

/* --------------------------------------------------------- who may ask --- */

// Reading the stream is watch; publishing the test event is control.
func TestEventsAreWatchAndTestIsControl(t *testing.T) {
	if needs("GET /v1/events") != RoleWatch || needs("POST /v1/events") != RoleControl {
		t.Fatalf("GET /v1/events takes %s and POST %s", needs("GET /v1/events"), needs("POST /v1/events"))
	}
	h := eventsServer(t)
	rd, _ := openEventStream(t, h.api.Handler(), "", bearer("watch-token-eeee"))
	say(h.api, events.SessionEnded, "watched")
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "watched" {
		t.Fatalf("a watch token's stream gave %+v", e)
	}
	h.token = "watch-token-eeee"
	if resp, raw := h.do("POST", "/v1/events", map[string]any{"type": "test"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a watch token's POST answered %d %s", resp.StatusCode, raw)
	}
	if resp := eventsGet(t, h.srv.URL, "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a stream without a credential answered %d", resp.StatusCode)
	}
}

// POST /v1/events takes the test event alone, and publishes it for the sink
// it names, in words that say who asked.
func TestATestEventIsPublished(t *testing.T) {
	h := eventsServer(t)
	resp, raw := h.do("POST", "/v1/events", map[string]any{"type": "test", "sink": "tg"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("the test event answered %d %s", resp.StatusCode, raw)
	}
	var doc struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.ID <= 0 {
		t.Fatalf("the answer is %s", raw)
	}
	got := published(h.api, events.Test)
	if len(got) != 1 {
		t.Fatalf("published %v", published(h.api))
	}
	e := got[0]
	if e.ID != doc.ID || e.Sink != "tg" || e.Name != "" || e.Text != "A test event from token" {
		t.Fatalf("the test event is %+v", e)
	}
	if resp, raw := h.do("POST", "/v1/events", map[string]any{"type": "test"}); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("a test event for every sink answered %d %s", resp.StatusCode, raw)
	}
	if e := published(h.api, events.Test)[1]; e.Sink != "" {
		t.Fatalf("a test event for every sink names %q", e.Sink)
	}

	only := `only a test event can be posted: {"type":"test","sink":"<name or empty for every sink>"}`
	for _, body := range []any{
		map[string]any{"type": "account.dead"},
		map[string]any{},
	} {
		resp, raw := h.do("POST", "/v1/events", body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), "only a test event can be posted") {
			t.Fatalf("%v answered %d %s, want 400: %s", body, resp.StatusCode, raw, only)
		}
	}
	for _, sink := range []string{strings.Repeat("x", 65), "a b", "-x", "x\n"} {
		if resp, raw := h.do("POST", "/v1/events", map[string]any{"type": "test", "sink": sink}); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("sink %q answered %d %s", sink, resp.StatusCode, raw)
		}
	}
	if resp, raw := h.do("POST", "/v1/events", strings.Repeat("x", 5000)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a body over 4 KiB answered %d %s", resp.StatusCode, raw)
	}
	if n := len(published(h.api, events.Test)); n != 2 {
		t.Fatalf("%d test events were published, want 2", n)
	}

	h.api.StopRefreshing()
	if resp, raw := h.do("POST", "/v1/events", map[string]any{"type": "test"}); resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(string(raw), "this server is stopping") {
		t.Fatalf("a test event while stopping answered %d %s", resp.StatusCode, raw)
	}
}

/* ------------------------------------------------------------ the words --- */

// What is published is safe to show anywhere: no credential, and no byte a
// terminal would act on, in any of its strings (Review Focus 2).
func TestEventTextCarriesNoSecret(t *testing.T) {
	h := eventsServer(t)
	key := "sk-ant-oat01-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	others := []string{"sk-proj-BBBBBBBBBBBBBBBBBBBBBBBB", "xai-CCCCCCCCCCCCCCCCCCCCCCCC"}
	dirty := "dead: " + key + " " + strings.Join(others, " ") + " \x1b]0;x\x07 \x9b2J \xff end"
	e := h.api.emit(events.Event{Type: events.AccountDead, Name: dirty, Text: dirty,
		Window: dirty, Viewer: dirty, Reason: dirty, Sink: dirty})
	kept := published(h.api, events.AccountDead)
	if len(kept) != 1 || kept[0].Text != e.Text {
		t.Fatalf("published %v, emit gave %+v", kept, e)
	}
	for field, v := range map[string]string{"name": e.Name, "text": e.Text, "window": e.Window,
		"viewer": e.Viewer, "reason": e.Reason, "sink": e.Sink} {
		for _, bad := range append([]string{"AAAAAAAAAAAAAAAA", "\x1b", "\x07", "\x9b", "\xff"}, others...) {
			if strings.Contains(v, bad) {
				t.Errorf("%s holds %q: %q", field, bad, v)
			}
		}
	}
	if !strings.HasPrefix(e.Text, "dead: sk-ant-[not repeated]") || !strings.HasSuffix(e.Text, " end") {
		t.Errorf("the text is %q", e.Text)
	}
	long := strings.Repeat("é", 1000)
	e = h.api.emit(events.Event{Type: events.SessionEnded, Name: long, Text: long})
	if len(e.Name) > 200 || len(e.Text) > 1000 || !strings.HasSuffix(e.Text, "…") {
		t.Errorf("a long name is %d bytes and a long text %d", len(e.Name), len(e.Text))
	}
	if e.Time.IsZero() || e.Time.Location() != time.UTC {
		t.Errorf("an event's time is %v, want now in UTC", e.Time)
	}
}

// A session is named in an event after what the person who started it
// called it, else its folder, else its id: never after its conversation or
// its title, which are the CLI's words and may be chat content.
func TestSessionNamesNeverComeFromTheConversation(t *testing.T) {
	held := &heldTerm{convName: "secret-chat", title: "✳ secret-title"}
	for _, c := range []struct {
		ts   *termSession
		want string
	}{
		{&termSession{id: "t1", name: "refactor", cwd: "/x/proj", back: held}, "refactor"},
		{&termSession{id: "t1", name: "  ", cwd: "/x/proj", back: held}, "proj"},
		{&termSession{id: "t1", cwd: "/x/proj/", back: held}, "proj"},
		{&termSession{id: "t1", back: held}, "t1"},
		{&termSession{id: "t1", cwd: "/"}, "t1"},
	} {
		if got := c.ts.eventName(); got != c.want {
			t.Errorf("name %q, cwd %q: named %q, want %q", c.ts.name, c.ts.cwd, got, c.want)
		}
	}
}

/* ------------------------------------------------------ not a daemon --- */

// A foreground server, one without HoldCommand, has the stream and says
// nothing of a daemon: neither as it starts nor as it stops.
func TestAForegroundServerHasTheStreamAndNoDaemonEvents(t *testing.T) {
	h := eventsServer(t)
	rd, _ := openEventStream(t, h.api.Handler(), "since=0", bearer(h.token))
	say(h.api, events.SessionEnded, "here")
	if e := nextEvent(t, rd, 2*time.Second); e.Text != "here" {
		t.Fatalf("the first event of a foreground server is %+v", e)
	}
	h.api.StopRefreshing()
	if _, err := nextOrErr(rd, 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("after StopRefreshing the stream gave %v", err)
	}
	if got := published(h.api, events.DaemonStarted, events.DaemonStopped); len(got) != 0 {
		t.Fatalf("a foreground server published %v", got)
	}
}
