package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/professor93/rota/internal/events"
	"github.com/professor93/rota/internal/holdwire"
	"github.com/professor93/rota/wire"
)

// The server's events: what it says has happened, kept in a bounded ring
// (internal/events) and read by whoever asks, over GET /v1/events. Every
// server has the ring and the route, a foreground one too; only a daemon
// says when it starts and stops, and only a daemon's sinks push the events
// anywhere (cmd/rota/notify, which reads this same stream).
//
// Publishing never waits on a reader. Each stream reads the ring at its own
// pace and is cut off when it cannot keep up with its socket; one that falls
// further behind than the ring keeps is told there is a gap.

var (
	// eventBeat is how often a stream with nothing to say says ": ping",
	// and also how often a page sign-in behind one is looked at again.
	eventBeat = 25 * time.Second
	// eventWrite bounds each write to a stream: a reader that has stopped
	// reading is let go of once its socket has been full for this long.
	eventWrite = 30 * time.Second
	// attendEvery is how often a daemon looks at its held sessions for
	// one that wants its person (attendAll).
	attendEvery = time.Second
	// newEventRing makes a server's ring, a seam so that a test can make it
	// small.
	newEventRing = events.NewRing
	// testEvery is how often a test event may be posted for one sink, so a
	// double click or a loop does not spend a sink's own rate limit, nor
	// push real events out of the ring.
	testEvery = time.Second
)

const (
	// eventRing is how many events a server keeps for a stream that asks
	// for what it missed.
	eventRing = 1024
	// eventStreams is how many streams a server holds open at once, the
	// local principal's not counted: those are the daemon's own sinks and
	// this machine's own user, and a crowd on the network must not be able
	// to switch them off.
	eventStreams = 64
	// eventStreamsEach is how many of those one sign-in may hold: a page
	// session, else a token or a user by name. So one client, an invite
	// left open on a tablet least of all, cannot take every place.
	eventStreamsEach = 16
	// eventBody bounds what POST /v1/events reads.
	eventBody = 4 << 10
)

// The longest each of an event's strings may be once emit has cleaned it.
const (
	eventNameMax  = 200
	eventTextMax  = 1000
	eventShortMax = 64
)

/* ------------------------------------------------------------ publishing --- */

// otherSecret is the shape of a credential of the other providers rota
// knows, standing as a word of its own: an OpenAI key (sk-, sk-proj-…) and
// an xAI key (xai-…). wire.Unsecret takes out Anthropic's.
var otherSecret = regexp.MustCompile(`(^|[^A-Za-z0-9])(sk|xai)-[A-Za-z0-9_-]{16,}`)

// escapeSeq is a whole terminal escape sequence: a CSI (ESC [, or U+009B)
// with its parameters and final byte, or an OSC up to its BEL or ST. Clean
// alone would blank only a CSI's ESC, and a key right after "\x1b[1m" would
// then follow the "m" as part of one word, which no key pattern matches.
var escapeSeq = regexp.MustCompile(`(?:\x1b\[|\x{9b})[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// cleanEventString is s fit to travel in an event: valid UTF-8 with no
// control byte, nothing shaped like a credential, and at most max bytes.
// Escape sequences go first, each as a space, so a key they were wrapped
// around stands as a word of its own and is masked. A control byte inside
// a key becomes a space, which splits it: what follows the space is no
// longer a key's shape, and is kept.
func cleanEventString(s string, max int) string {
	s = escapeSeq.ReplaceAllString(s, " ")
	s = events.Clean(s, 0)
	s = wire.Unsecret(s)
	s = otherSecret.ReplaceAllString(s, "${1}${2}-[not repeated]")
	return events.Clean(s, max)
}

// emit publishes e as having happened now, and returns it as published,
// with its id; once the ring is closed it publishes nothing and returns e
// with no id. Every string a person could read is cleaned first: no control
// byte, no credential, and bounded. It may be called with any lock held: it
// takes only the ring's, never blocks on a reader, and calls nothing back.
func (s *Server) emit(e events.Event) events.Event {
	if s.events == nil {
		return e // a server New did not make, as some tests build
	}
	e.Time = s.now().UTC()
	e.Name = cleanEventString(e.Name, eventNameMax)
	e.Text = cleanEventString(e.Text, eventTextMax)
	e.Window = cleanEventString(e.Window, eventShortMax)
	e.Viewer = cleanEventString(e.Viewer, eventShortMax)
	e.Reason = cleanEventString(e.Reason, eventShortMax)
	e.Sink = cleanEventString(e.Sink, eventShortMax)
	out, _ := s.events.Publish(e)
	return out
}

// eventName is what a session is called in an event: what the person who
// started it called it, else its folder, else its id. Never its
// conversation's name or its title: those are the CLI's words, and may be
// what somebody typed into it.
func (ts *termSession) eventName() string {
	if n := strings.TrimSpace(ts.name); n != "" {
		return n
	}
	if f := ts.folder(); f != "" {
		return f
	}
	return ts.id
}

/* ---------------------------------------------------- the daemon's own --- */

// daemonStarted says a daemon has started, with how many sessions it took
// up and how many stopped ones its store lists. New calls it as soon as the
// store's records are swept, so the counts are what it found; an adopted
// session that ends at once may still be said before it. A server that is
// not a daemon says nothing.
func (s *Server) daemonStarted() {
	if s.opts.HoldCommand == nil {
		return
	}
	running := 0
	for _, ts := range s.everyTerminal() {
		if !ts.over() {
			running++
		}
	}
	stopped := 0
	for _, r := range holdwire.Scan(s.terminals) {
		if r.State == holdwire.StateEnded || r.State == holdwire.StateInterrupted {
			stopped++
		}
	}
	s.emit(events.Event{Type: events.DaemonStarted, Text: fmt.Sprintf(
		"The rota daemon %s started for %s with %s running and %d stopped",
		wire.Version, s.storeDir(), counted(running, "session"), stopped)})
}

// closeEvents ends the server's events, once: a daemon says it is stopping,
// with how many of its sessions go on without it, and then the ring takes
// nothing more, which ends every stream after what it has not yet read.
func (s *Server) closeEvents() {
	s.closeOnce.Do(func() {
		if s.opts.HoldCommand != nil {
			going := 0
			for _, ts := range s.everyTerminal() {
				if ts.held() && !ts.over() {
					going++
				}
			}
			text := fmt.Sprintf("The rota daemon for %s is stopping; no session is running", s.storeDir())
			switch {
			case going == 1:
				text = fmt.Sprintf("The rota daemon for %s is stopping; its 1 running session goes on", s.storeDir())
			case going > 1:
				text = fmt.Sprintf("The rota daemon for %s is stopping; its %d running sessions go on", s.storeDir(), going)
			}
			s.emit(events.Event{Type: events.DaemonStopped, Text: text})
		}
		s.events.Close()
	})
}

// storeDir is the store this server serves, as a person would name it.
func (s *Server) storeDir() string {
	if s.opts.Dir != "" {
		return s.opts.Dir
	}
	if s.terminals != "" {
		return filepath.Dir(s.terminals)
	}
	return "the default store"
}

// counted is "1 session" or "3 sessions".
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// keepAttending looks at the held sessions every attendEvery until the
// sweeps end, which is when shutdown begins. A panic in one look costs that
// look, not the daemon.
func (s *Server) keepAttending() {
	t := time.NewTicker(attendEvery)
	defer t.Stop()
	for {
		select {
		case <-s.sweeps.Done():
			return
		case <-t.C:
		}
		func() {
			defer func() {
				if v := recover(); v != nil {
					s.log.Error("panic while looking at the held sessions' attention", "panic", v)
				}
			}()
			s.attendAll(s.now())
		}()
	}
}

/* ------------------------------------------------------------ the stream --- */

// streamEvents is GET /v1/events: the server's events as server-sent
// events, which a browser's EventSource reads as they are.
//
// It starts after Last-Event-ID, else after ?since, else from now; on is a
// comma-separated list of patterns (events.Match). A stream asked for events
// older than the ring keeps is given a gap first. It lasts until the reader
// goes, its write does not finish within eventWrite, its page sign-in is
// over (looked at each heartbeat), or shutdown begins, when it ends after
// the events it has not yet been given.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	last := s.events.Last()
	from := r.URL.Query().Get("since")
	if id := r.Header.Get("Last-Event-ID"); id != "" {
		from = id
	}
	if from != "" {
		id, err := strconv.ParseInt(from, 10, 64)
		if err != nil || id < 0 {
			fail(w, http.StatusBadRequest, "since must be an event id")
			return
		}
		last = id
	}
	var on []string
	if raw := r.URL.Query().Get("on"); raw != "" {
		for _, pat := range strings.Split(raw, ",") {
			if pat = strings.TrimSpace(pat); pat != "" {
				on = append(on, pat)
			}
		}
		if len(on) == 0 {
			fail(w, http.StatusBadRequest, "on names no event")
			return
		}
		if err := events.CheckPatterns(on); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	release, refused := s.takeStream(p)
	if refused != "" {
		fail(w, http.StatusServiceUnavailable, refused)
		return
	}
	defer release()

	beatEvery, writeFor := eventBeat, eventWrite
	rc := http.NewResponseController(w)
	// The request has been read; what is left of the connection is this
	// answer, which lasts. A server's ReadTimeout does not reach a response
	// on HTTP/1.1 or h2, since nothing reads after this, so clearing the
	// read deadline is belt and braces. A WriteTimeout would end the stream;
	// the deadline set before each write below takes its place.
	_ = rc.SetReadDeadline(time.Time{})
	// Whatever ends the stream, the connection is left with no deadline,
	// so a request after it on the same connection is not cut short.
	defer rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // a proxy in front must not hold the stream back
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	var buf bytes.Buffer
	send := func() bool {
		if buf.Len() == 0 {
			return true
		}
		_ = rc.SetWriteDeadline(time.Now().Add(writeFor))
		_, err := w.Write(buf.Bytes())
		buf.Reset()
		if err == nil {
			if err = rc.Flush(); errors.Is(err, http.ErrNotSupported) {
				err = nil // a writer that cannot flush sends as it goes
			}
		}
		return err == nil
	}
	buf.WriteString(events.Retry)
	if !send() {
		return
	}

	beat := time.NewTicker(beatEvery)
	defer beat.Stop()
	closed := false
	for {
		evs, gap, wake := s.events.Since(last)
		if gap {
			// A gap is no place in the stream: last moves only with events.
			buf.Write(events.GapFrame(evs[0].ID))
		}
		for _, e := range evs {
			last = e.ID
			if events.Match(on, e.Type) {
				buf.Write(events.Frame(e))
			}
		}
		if !send() {
			return
		}
		if len(evs) > 0 {
			continue
		}
		if closed {
			return // every event the ring took has been given
		}
		select {
		case <-s.events.Closed():
			closed = true // once more, for what came with the close
			continue
		default:
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.events.Closed():
			closed = true
		case <-wake:
		case <-beat.C:
			if !s.stillSignedIn(p) {
				return
			}
			buf.WriteString(events.Heartbeat)
			if !send() {
				return
			}
		}
	}
}

// takeStream takes a place for one stream of p's, and gives the function
// that gives it back; or, when there is none, the sentence it is refused
// with. The local principal, the control socket's (the daemon's own sinks,
// and this machine's user), takes no place.
func (s *Server) takeStream(p *Principal) (release func(), refused string) {
	if p.Via == "local" {
		return func() {}, ""
	}
	key := p.session
	if key == "" {
		key = p.Via + "\x00" + p.Name
	}
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	switch {
	case s.streams.Load() >= eventStreams:
		return nil, "too many event streams are open"
	case s.streamsBy[key] >= eventStreamsEach:
		return nil, "too many event streams are open for this sign-in"
	}
	if s.streamsBy == nil {
		s.streamsBy = map[string]int{}
	}
	s.streamsBy[key]++
	s.streams.Add(1)
	return func() {
		s.streamsMu.Lock()
		defer s.streamsMu.Unlock()
		if s.streamsBy[key]--; s.streamsBy[key] <= 0 {
			delete(s.streamsBy, key)
		}
		s.streams.Add(-1)
	}, ""
}

// stillSignedIn reports whether the principal a stream was opened by is
// still who it was: a page sign-in, a person's or an invite's, ends with
// its session, signed out or expired. A bearer token and the local
// principal have no session, and last as long as the file or the socket.
func (s *Server) stillSignedIn(p *Principal) bool {
	if p.session == "" {
		return true
	}
	if !p.Expires.IsZero() && !time.Now().Before(p.Expires) {
		return false
	}
	return s.sessions.get(p.session) != nil
}

/* -------------------------------------------------------- the test event --- */

// testSinkName is what a sink may be called, as server.toml has it.
var testSinkName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// onlyTest is the one thing POST /v1/events takes, said as a refusal.
const onlyTest = `only a test event can be posted: {"type":"test","sink":"<name or empty for every sink>"}`

// postEvent is POST /v1/events: the test button. It publishes a test event
// for the sink it names, or for every sink, and the daemon's sinks send it
// whatever their on says. It answers 202 with the event's id, since sending
// it is the sinks' to do.
func (s *Server) postEvent(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, eventBody))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		fail(w, http.StatusBadRequest, fmt.Sprintf("a test event is at most %d bytes", eventBody))
		return
	}
	var in struct {
		Type string `json:"type"`
		Sink string `json:"sink"`
	}
	if err != nil || strictJSON(bytes.NewReader(raw), &in) != nil || in.Type != events.Test {
		fail(w, http.StatusBadRequest, onlyTest)
		return
	}
	if in.Sink != "" && !testSinkName.MatchString(in.Sink) {
		fail(w, http.StatusBadRequest, "sink must be a sink's name, or empty for every sink")
		return
	}
	select {
	case <-s.events.Closed():
		fail(w, http.StatusServiceUnavailable, "this server is stopping")
		return
	default:
	}
	if !s.mayTest(in.Sink) {
		what := in.Sink
		if what == "" {
			what = "every sink"
		}
		fail(w, http.StatusTooManyRequests, "a test event for "+what+" was sent a moment ago")
		return
	}
	e := s.emit(events.Event{Type: events.Test, Sink: in.Sink, Text: "A test event from " + who(r).Name})
	if e.ID == 0 {
		fail(w, http.StatusServiceUnavailable, "this server is stopping")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": e.ID})
}

// mayTest reports whether a test event may be posted for sink now, and if
// so counts it as posted: at most one each testEvery for one sink, and for
// every sink at once (""). What it remembers is pruned as it grows, so a
// caller naming a new sink each time costs nothing lasting.
func (s *Server) mayTest(sink string) bool {
	now := time.Now()
	s.testedMu.Lock()
	defer s.testedMu.Unlock()
	if at, ok := s.tested[sink]; ok && now.Sub(at) < testEvery {
		return false
	}
	if s.tested == nil {
		s.tested = map[string]time.Time{}
	}
	if len(s.tested) >= 64 {
		for k, at := range s.tested {
			if now.Sub(at) >= testEvery {
				delete(s.tested, k)
			}
		}
	}
	s.tested[sink] = now
	return true
}
