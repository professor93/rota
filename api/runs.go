package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
	"github.com/professor93/rota/wire"
)

// heartbeat is how often a reader with nothing to read is sent something
// anyway, so a proxy with an idle timeout and a client waiting on a socket
// both go on believing the run is there. It is a variable so a test need not
// wait fifteen seconds to see one.
var heartbeat = 15 * time.Second

// keepEnded is how long a finished run stays addressable. A reader that
// reattaches a moment too late should be given the tail of the stream and the
// event that closed it, rather than a 404 it cannot tell from a wrong id.
const keepEnded = 5 * time.Minute

// liveRun is one run that stays open, and everything the endpoints need to
// reach it.
//
// A run like this outlives the request that started it: that response is only
// the first reader, it may go away and come back, and another may replace it.
// So the events are kept here rather than written straight out, and every
// door into the run goes through this one place.
type liveRun struct {
	id       string
	account  int
	provider string
	sess     *rota.Session
	since    time.Time

	// learn is told the conversation this run turned out to be in, once.
	learn func(string)
	// graceFor is how long the run goes on with nobody reading it, and keep
	// how many events it remembers for whoever reads it next.
	graceFor time.Duration
	keep     int

	// mu guards everything below it, and is held across message.Stream's own
	// calls — which is why the stream needs no lock of its own. A run has
	// three sources: the CLI's output, read on one goroutine, what became of
	// each message sent in, read on another, and the terminal event on a
	// third. Unguarded they would interleave halves of a line and race over
	// the sequence numbers.
	mu     sync.Mutex
	stream message.Stream
	// ring is the last keep events, oldest first from start. A slice that
	// dropped its head would copy every event it keeps on every event it
	// takes; this copies none.
	ring  []message.Event
	start int

	reader *runReader
	grace  *time.Timer

	sessionID string
	// idle is what the last notice said: everything sent has been answered.
	idle  bool
	ended bool
	end   wire.End
}

// runID names one open run: sixteen hex characters from crypto/rand. It is
// what a request sends a message into somebody's agent by, so it is not
// something anyone could guess or count up to.
func runID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

func (s *Server) newLiveRun(a *rota.Account, with message.With, tally *message.Tally) *liveRun {
	lr := &liveRun{
		id: runID(), account: a.ID, provider: a.Provider, since: time.Now(),
		graceFor: s.opts.InputGrace, keep: s.opts.Replay,
	}
	lr.stream = message.Stream{Account: a.ID, Provider: a.Provider, With: with, Tally: tally, Emit: lr.emit}
	return lr
}

/* ------------------------------------------------- the doors into a run --- */

// Write is the first door: whatever the CLI printed, turned into events. It
// is the writer the session was started with.
func (lr *liveRun) Write(p []byte) (int, error) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.stream.Write(p)
}

// Send is the second: an event of rota's own — the opening one, and what
// became of each message sent into the run.
func (lr *liveRun) Send(ev message.Event) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	_ = lr.stream.Send(ev)
}

// finish is the third and last: the run ended, and says how. The terminal
// event is not one of the stream's — it carries the result, not a happening —
// so it is kept as itself, for a reader that arrives after everything.
func (lr *liveRun) finish(end wire.End) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	_ = lr.stream.Rest() // a last line with no newline after it is still an event
	lr.ended, lr.end = true, end
	if lr.grace != nil {
		lr.grace.Stop()
		lr.grace = nil
	}
	if rd := lr.reader; rd != nil {
		if raw, err := rota.Encode(end); err == nil {
			_ = rd.frame(end.Type, 0, raw)
		}
		lr.reader = nil
		rd.close()
	}
}

// emit is where every event of this run goes: into the ring for whoever reads
// it next, and out to the reader reading it now. It is message.Stream's Emit,
// so everything arrives here already numbered, and every path to it holds mu.
func (lr *liveRun) emit(ev message.Event) error {
	if ev.SessionID != "" {
		if lr.sessionID == "" && lr.learn != nil {
			lr.learn(ev.SessionID)
		}
		lr.sessionID = ev.SessionID
	}
	if len(lr.ring) < lr.keep {
		lr.ring = append(lr.ring, ev)
	} else {
		lr.ring[lr.start] = ev
		lr.start = (lr.start + 1) % len(lr.ring)
	}
	raw, err := rota.Encode(ev)
	if err != nil {
		return err
	}
	lr.write(ev.Type, ev.Seq, raw)
	return nil
}

// write puts one framed event in front of the reader attached now. A write
// that fails is a reader that has gone, and it is let go here rather than
// left for the heartbeat to discover.
func (lr *liveRun) write(name string, seq int, raw []byte) {
	if lr.reader == nil {
		return
	}
	if err := lr.reader.frame(name, seq, raw); err != nil {
		lr.drop()
	}
}

/* ------------------------------------------------------ readers and time --- */

// attach makes rd the run's reader, replacing whoever was reading, and gives
// it everything kept after the event it names.
func (lr *liveRun) attach(rd *runReader, since int) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.reader != nil && lr.reader != rd {
		// One reader at a time: the events are a stream, and two responses
		// taking turns at it would each see half of one.
		lr.reader.close()
	}
	if lr.grace != nil {
		lr.grace.Stop()
		lr.grace = nil
	}
	lr.reader = rd
	for i := range lr.ring {
		ev := lr.ring[(lr.start+i)%len(lr.ring)]
		if ev.Seq <= since {
			continue
		}
		raw, err := rota.Encode(ev)
		if err != nil {
			continue
		}
		if err := rd.frame(ev.Type, ev.Seq, raw); err != nil {
			lr.drop()
			return
		}
	}
	if !lr.ended {
		return
	}
	// Nothing more is coming: say how it ended and let the reader go, rather
	// than holding a request open on a run that is over.
	if raw, err := rota.Encode(lr.end); err == nil {
		_ = rd.frame(lr.end.Type, 0, raw)
	}
	lr.reader = nil
	rd.close()
}

// detach is the client going away. The run keeps going, alone, for the grace
// period: a dropped connection is usually a client about to come back, and
// killing the agent for it would throw away the conversation it holds.
func (lr *liveRun) detach(rd *runReader) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.reader == rd {
		lr.drop()
	}
}

// drop lets the current reader go and starts the clock on an unread run. It
// is called under mu.
func (lr *liveRun) drop() {
	if lr.reader == nil {
		return
	}
	lr.reader.close()
	lr.reader = nil
	if lr.ended {
		return
	}
	if lr.graceFor < 0 {
		go lr.shut()
		return
	}
	if lr.grace != nil {
		lr.grace.Stop()
	}
	lr.grace = time.AfterFunc(lr.graceFor, func() {
		lr.mu.Lock()
		alone := lr.reader == nil && !lr.ended
		lr.mu.Unlock()
		if alone {
			lr.shut()
		}
	})
}

// shut ends a run nobody is reading: whatever the agent is doing is stopped,
// and then it is told there is nothing more, so it finishes its turn, exits,
// and lets go of the account it was spending.
func (lr *liveRun) shut() {
	if lr.sess == nil {
		return
	}
	_, _ = lr.sess.Interrupt()
	_ = lr.sess.Close()
}

// beat sends the heartbeat, if this reader is still the one attached.
func (lr *liveRun) beat(rd *runReader) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.reader != rd {
		return
	}
	if err := rd.ping(); err != nil {
		lr.drop()
	}
}

// serve attaches this response to the run and holds the request open until
// the run ends or the client goes away.
func (lr *liveRun) serve(w http.ResponseWriter, r *http.Request, since int) {
	ndjson := wantsNDJSON(r)
	streamHeaders(w, ndjson)
	rd := newRunReader(w, !ndjson)
	lr.attach(rd, since)
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-rd.gone:
			return
		case <-r.Context().Done():
			lr.detach(rd)
			return
		case <-t.C:
			lr.beat(rd)
		}
	}
}

// over reports whether the run has ended, which is the one thing every
// endpoint has to know before it tries anything.
func (lr *liveRun) over() bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.ended
}

// view is what a run says about itself.
func (lr *liveRun) view() map[string]any {
	pending := 0
	if lr.sess != nil {
		pending = lr.sess.Pending()
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	state := "running"
	switch {
	case lr.ended:
		state = "ended"
	case pending == 0 && lr.idle:
		state = "idle"
	}
	return map[string]any{
		"id": lr.id, "account": lr.account, "provider": lr.provider,
		"session_id": lr.sessionID, "state": state, "pending": pending,
		"attached": lr.reader != nil, "since": lr.since.Format(time.RFC3339),
		"events": lr.stream.Seq(),
	}
}

// notices carries what became of each message into the stream the client is
// already reading, and keeps the run's own idea of whether it is idle.
func (lr *liveRun) notices() {
	for n := range lr.sess.Notices() {
		lr.mu.Lock()
		switch n.Kind {
		case "idle":
			lr.idle = true
		case "accepted":
			lr.idle = false
		}
		_ = lr.stream.Send(message.FromNotice(n))
		lr.mu.Unlock()
	}
}

/* -------------------------------------------------------------- readers --- */

// runReader is one reader of a run — an HTTP response or a socket — in the
// framing it wanted, and the channel that says it is no longer the one
// reading.
type runReader struct {
	w    io.Writer
	sse  bool
	ws   *wsConn // set when the reader is a WebSocket: one event, one text frame
	buf  []byte
	gone chan struct{}
	once sync.Once
}

func newRunReader(w io.Writer, sse bool) *runReader {
	return &runReader{w: w, sse: sse, gone: make(chan struct{})}
}

// newWSReader reads a run onto a socket, where the framing is the protocol's
// own: each event is one text frame carrying the same JSON the NDJSON stream
// carries, and nothing is added around it.
func newWSReader(c *wsConn) *runReader {
	return &runReader{ws: c, gone: make(chan struct{})}
}

func (rd *runReader) close() { rd.once.Do(func() { close(rd.gone) }) }

// frame writes one event in this reader's framing. An SSE frame carries the
// sequence number as its id, so a browser that reconnects on its own says in
// Last-Event-ID where it got to and is answered from there.
func (rd *runReader) frame(name string, seq int, raw []byte) error {
	if rd.ws != nil {
		return rd.ws.send(opText, raw)
	}
	rd.buf = rd.buf[:0]
	if rd.sse {
		rd.buf = append(rd.buf, "event: "...)
		rd.buf = append(rd.buf, name...)
		if seq > 0 {
			rd.buf = append(rd.buf, "\nid: "...)
			rd.buf = strconv.AppendInt(rd.buf, int64(seq), 10)
		}
		rd.buf = append(rd.buf, "\ndata: "...)
	}
	rd.buf = append(rd.buf, raw...)
	if rd.sse {
		rd.buf = append(rd.buf, '\n')
	}
	rd.buf = append(rd.buf, '\n')
	_, err := rd.w.Write(rd.buf)
	flush(rd.w)
	return err
}

// ping is the heartbeat: an SSE comment, which a client is required to
// ignore, and on NDJSON an event with no sequence number — a reader counting
// events must skip it rather than read it as a gap. A socket has a ping of
// its own, which the peer answers, so it is also how a dead one is noticed.
func (rd *runReader) ping() error {
	if rd.ws != nil {
		return rd.ws.send(opPing, nil)
	}
	line := []byte("{\"type\":\"ping\"}\n")
	if rd.sse {
		line = []byte(": ping\n\n")
	}
	_, err := rd.w.Write(line)
	flush(rd.w)
	return err
}

/* ------------------------------------------------------- the registry --- */

func (s *Server) addRun(lr *liveRun) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	s.runs[lr.id] = lr
}

func (s *Server) findRun(id string) *liveRun {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	return s.runs[id]
}

func (s *Server) dropRun(id string) {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	delete(s.runs, id)
}

func (s *Server) everyRun() []*liveRun {
	s.runsMu.Lock()
	defer s.runsMu.Unlock()
	out := make([]*liveRun, 0, len(s.runs))
	for _, lr := range s.runs {
		out = append(out, lr)
	}
	return out
}

/* ------------------------------------------------------- the endpoints --- */

// runOf finds the run the path names, answering for an id nobody knows.
func (s *Server) runOf(w http.ResponseWriter, r *http.Request) (*liveRun, bool) {
	id := r.PathValue("id")
	lr := s.findRun(id)
	if lr == nil {
		fail(w, http.StatusNotFound, "no run "+id)
		return nil, false
	}
	return lr, true
}

// gone is the answer for a run that is over: the id is right, the run is not
// there to take anything.
func (s *Server) gone(w http.ResponseWriter, lr *liveRun) {
	fail(w, http.StatusConflict, "run "+lr.id+" has ended")
}

// runSend puts one more message into a run, or steers it: interrupting first,
// so the message starts the next turn instead of joining the one being
// abandoned.
func (s *Server) runSend(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	var body struct {
		Text  string `json:"text"`
		Steer bool   `json:"steer"`
	}
	if err := decodeJSON(r, &body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if lr.over() {
		s.gone(w, lr)
		return
	}
	send := lr.sess.Send
	if body.Steer {
		send = lr.sess.Steer
	}
	id, err := send(body.Text)
	if err != nil {
		if errors.Is(err, rota.ErrClosed) {
			s.gone(w, lr)
			return
		}
		s.report(w, r, err)
		return
	}
	// Accepted, not done: the line has reached the CLI's stdin, and what the
	// agent makes of it arrives in the run's own stream as an input event.
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": "accepted"})
}

// runInterrupt stops the tool the agent is running. The acknowledgement
// arrives in the run's stream, carrying this id.
func (s *Server) runInterrupt(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	if lr.over() {
		s.gone(w, lr)
		return
	}
	id, err := lr.sess.Interrupt()
	if err != nil {
		if errors.Is(err, rota.ErrClosed) {
			s.gone(w, lr)
			return
		}
		s.report(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id})
}

// runClose says there is nothing more: the CLI finishes the turn it is on and
// exits. It is idempotent, because a caller that says it twice means it once.
func (s *Server) runClose(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	if !lr.over() && lr.sess != nil {
		if err := lr.sess.Close(); err != nil && !errors.Is(err, rota.ErrClosed) {
			s.report(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) describeRun(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, lr.view())
}

func (s *Server) listRuns(w http.ResponseWriter, _ *http.Request) {
	runs := s.everyRun()
	out := make([]map[string]any, 0, len(runs))
	for _, lr := range runs {
		out = append(out, lr.view())
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// runEvents reattaches a reader to a run, replaying what it missed.
//
// It is the run's own stream, in the same two framings, so a client that
// dropped its connection carries on reading exactly what it was reading —
// from where it got to, which it says in since or, for a browser reconnecting
// on its own, in Last-Event-ID.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	lr, ok := s.runOf(w, r)
	if !ok {
		return
	}
	since, ok := sinceOf(w, r)
	if !ok {
		return
	}
	lr.serve(w, r, since)
}

// sinceOf is where a reader says it got to. No answer means everything still
// kept; a browser reconnecting on its own says it in Last-Event-ID instead.
func sinceOf(w http.ResponseWriter, r *http.Request) (int, bool) {
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, "since must be a sequence number")
			return 0, false
		}
		return n, true
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, true
}
