package rota

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"io"
	"strings"
	"sync"
)

// Session is a run that stays open for more messages after its first answer.
//
// Run writes the prompt, closes the CLI's stdin and waits. A Session keeps
// stdin open instead, so the caller can send another message into the same
// process — the same conversation, the same context, no resume — and can
// interrupt whatever the agent is doing. One goroutine owns stdin and every
// line goes through it, so two callers sending at once cannot interleave
// halves of a message.
//
// What happens to a message is reported on Notices. A message is *accepted*
// when its line has reached the CLI's stdin, and *answered* when the CLI has
// finished a turn that carried it. The two are separate because the CLI never
// echoes an injected message back: the one thing it does say is
// queued_turn_count on each result, how many messages it has taken but not
// yet made turns of. At a result carrying zero, everything accepted before it
// has been answered; at a result carrying N, the last N are still waiting.
// That count is what Pending reports and what answered is derived from.
//
// Only Claude Code has a streaming input today, so Spec.Input is claude
// vocabulary and every other CLI refuses it by name.
type Session struct {
	l *launched
	// out carries lines to the one goroutine that writes them; a nil line is
	// the marker that closes stdin.
	out     chan *outbound
	notices chan Notice
	done    chan struct{}

	// sendMu serializes callers, so the order messages are recorded in is the
	// order they reach stdin. It is held across a write and is never held
	// with mu.
	sendMu sync.Mutex

	// mu guards everything below it.
	mu sync.Mutex
	// pending are the message ids accepted and not yet answered, in send
	// order: the last N of them are the ones a result says are still queued.
	pending []string
	closed  bool
	// writeErr is why the session closed itself, when a write failed.
	writeErr error
	// dropped counts notices a consumer was too slow to take. The CLI is
	// never held up for a channel nobody is draining.
	dropped  int
	finished bool
	res      *Result
	err      error
}

// Notice is one thing that happened to a message or an interrupt.
type Notice struct {
	Kind string // "accepted", "answered", "interrupted", "failed", "idle"
	ID   string // the message id, or the interrupt's request id; empty for idle
	Err  string // for failed
}

// outbound is one line on its way to stdin, and the answer the caller waits
// for. A nil line closes stdin.
type outbound struct {
	line []byte
	done chan error
}

const (
	// maxMessage bounds one message. A prompt this size is already far past
	// what a turn can use, and the bound keeps a mistake from filling the
	// CLI's pipe.
	maxMessage = 64 << 10
	// maxPending is how many messages may be accepted and unanswered at once.
	// Past that the agent is not keeping up and the caller should wait rather
	// than queue more.
	maxPending = 100
	// noticeBuffer is how far a consumer may fall behind before notices are
	// dropped instead of stalling the run.
	noticeBuffer = 1024
	// outBuffer is the writer's queue. It exists so a caller hands over a
	// line rather than waiting for the one in front of it to reach the pipe.
	outBuffer = 1024
)

// Start launches an account's CLI as a session and returns once it is
// running with the prompt delivered.
//
// The spec must ask for both Input and Stream: the CLI's streaming input goes
// with its streaming output, and a run that takes more messages has events to
// report while it does. Everything else — the model, the tools, the
// confinement — is the spec Run would have used, checked the same way.
//
// The caller owns the session until it ends: Close shuts stdin, Wait returns
// the result, and cancelling ctx kills the CLI and its children. A consumer
// must drain Notices, or notices are dropped.
func Start(ctx context.Context, a *Account, home string, cmd *Command, spec Spec, lim *Limits, events io.Writer) (*Session, error) {
	var missing []string
	if !spec.Input {
		missing = append(missing, "input")
	}
	if !spec.Stream {
		missing = append(missing, "stream")
	}
	if len(missing) > 0 {
		return nil, failf(ErrInvalidRequest,
			"a session needs %s: Start keeps the CLI open for more messages and reads its events as they arrive",
			strings.Join(missing, " and "))
	}
	l, err := launch(ctx, a, home, cmd, &spec, lim)
	if err != nil {
		spec.cleanup()
		return nil, err
	}
	s := &Session{
		l:       l,
		out:     make(chan *outbound, outBuffer),
		notices: make(chan Notice, noticeBuffer),
		done:    make(chan struct{}),
	}
	go s.writer()
	// The prompt is the first message, in the same shape as every message
	// after it: with a streaming input the CLI takes JSON lines, not text.
	line, err := messageLine(spec.Prompt)
	if err == nil {
		err = s.hand(line)
	}
	if err != nil {
		// Nothing was ever said to this child, so there is no result to keep:
		// stop it, let go of what the launch took, and report the failure.
		_ = s.hand(nil) // ends the writer, which owns stdin
		killGroup(l.child)
		_ = l.child.Wait()
		l.stop()
		l.done()
		spec.cleanup()
		return nil, err
	}
	go func() {
		// One reader for the whole session: the stream is read as any stream
		// is, and the tap takes what the lines say about the messages sent.
		scanErr := readOutput(l.stdout, &tap{out: events, s: s}, true, spec.IncludeEvents, l.cp, l.res)
		res, err := finishRun(l, scanErr)
		l.stop()
		l.done()
		spec.cleanup()
		s.finish(res, err)
	}()
	return s, nil
}

// Send delivers one more message into the running session and returns the id
// its notices will carry. It returns once the line has reached stdin; what
// the agent does with it arrives later, as an answered notice.
func (s *Session) Send(text string) (string, error) {
	if text == "" {
		return "", failf(ErrInvalidRequest, "a message with no text is nothing to send")
	}
	if len(text) > maxMessage {
		return "", failf(ErrInvalidRequest, "message is %d bytes, and one message is capped at %d", len(text), maxMessage)
	}
	line, err := messageLine(text)
	if err != nil {
		return "", err
	}
	id := randHex(8)
	if err := s.deliver(id, line, true); err != nil {
		return "", err
	}
	return id, nil
}

// SendRaw writes one line the caller composed itself, in the CLI's own input
// vocabulary, for a caller that speaks that shape directly — a transport
// handing on what its own client wrote, say.
//
// Nothing is tracked: the line gets no id and no notices, because rota did
// not make it and cannot tell what the CLI will consider it. What is checked
// is only what protects the pipe: one JSON object, and not a huge one. A
// caller that wants delivery reported sends text through Send.
func (s *Session) SendRaw(line []byte) error {
	raw := bytes.TrimSpace(line)
	if len(raw) > maxMessage {
		return failf(ErrInvalidRequest, "the line is %d bytes, and one line is capped at %d", len(raw), maxMessage)
	}
	if len(raw) == 0 || raw[0] != '{' || !jsontext.Value(raw).IsValid() {
		return failf(ErrInvalidRequest, "a raw line must be one JSON object in the CLI's own input vocabulary")
	}
	out := append(bytes.Clone(raw), '\n')

	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	if s.closed {
		err := s.closedErr()
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	err := s.hand(out)
	if err == nil {
		return nil
	}
	// As in deliver: a pipe that cannot be written to is a session that is
	// over, for this caller and for everyone after it.
	s.mu.Lock()
	s.closed, s.writeErr = true, err
	s.mu.Unlock()
	return failf(ErrClosed, "the session closed: %v", err)
}

// Steer interrupts whatever the agent is doing and sends a message, which is
// how a caller redirects a run rather than waiting it out. The interrupt goes
// down the pipe first, so the message starts the next turn instead of being
// folded into the one being abandoned. It returns the message's id; the
// interrupt's own id arrives on Notices as interrupted.
func (s *Session) Steer(text string) (string, error) {
	if _, err := s.Interrupt(); err != nil {
		return "", err
	}
	return s.Send(text)
}

// Interrupt asks the CLI to stop the tool it is running and returns the
// request id. The CLI acknowledges every interrupt, idle or not, and that
// acknowledgement arrives on Notices as interrupted with this id.
func (s *Session) Interrupt() (string, error) {
	id := randHex(8)
	line, err := interruptLine(id)
	if err != nil {
		return "", err
	}
	if err := s.deliver(id, line, false); err != nil {
		return "", err
	}
	return id, nil
}

// Close shuts the session's stdin, which is how a CLI is told there is
// nothing more: it finishes the turn it is on and exits. It is idempotent,
// and after it Send, Steer and Interrupt return ErrClosed. Wait is what waits
// for the exit.
func (s *Session) Close() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	already := s.closed
	s.closed = true
	s.mu.Unlock()
	if already {
		return nil
	}
	return s.hand(nil)
}

// Notices reports what became of each message and interrupt. It is buffered
// and a consumer must drain it: a full channel drops notices rather than
// holding up the CLI. It is closed when the session ends.
func (s *Session) Notices() <-chan Notice { return s.notices }

// Done is closed when the CLI has exited and the result is ready.
func (s *Session) Done() <-chan struct{} { return s.done }

// Wait blocks until the CLI has exited and returns what the run produced —
// the same result and verdict Run would have given.
func (s *Session) Wait() (*Result, error) {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res, s.err
}

// Pending is how many messages have been accepted and not yet answered.
func (s *Session) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// deliver records a line, writes it, and says what happened. track marks a
// message, which is owed an answer; an interrupt is not.
func (s *Session) deliver(id string, line []byte, track bool) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	s.mu.Lock()
	if s.closed {
		err := s.closedErr()
		s.mu.Unlock()
		return err
	}
	if track {
		if n := len(s.pending); n >= maxPending {
			s.mu.Unlock()
			return failf(ErrBusy, "%d messages are already waiting for an answer, which is the limit; let the agent catch up", n)
		}
		// Recorded before it is written, because the result that answers it
		// can be read the instant the CLI has it.
		s.pending = append(s.pending, id)
	}
	s.mu.Unlock()

	err := s.hand(line)
	if err == nil {
		if track {
			s.emit(Notice{Kind: "accepted", ID: id})
		}
		return nil
	}
	// A pipe that cannot be written to is a session that is over: say so once
	// here, and to everyone who sends afterwards.
	s.mu.Lock()
	s.closed, s.writeErr = true, err
	for i, p := range s.pending {
		if p == id {
			s.pending = append(s.pending[:i], s.pending[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	s.emit(Notice{Kind: "failed", ID: id, Err: err.Error()})
	return failf(ErrClosed, "the session closed: %v", err)
}

// hand gives one line to the writer and waits for it to reach stdin, so a
// caller hears about a broken pipe from the call that broke it.
func (s *Session) hand(line []byte) error {
	o := &outbound{line: line, done: make(chan error, 1)}
	s.out <- o
	return <-o.done
}

// writer is the only goroutine that touches stdin.
func (s *Session) writer() {
	for o := range s.out {
		if o.line == nil {
			o.done <- s.l.stdin.Close()
			return // nothing is sent after the close marker
		}
		_, err := s.l.stdin.Write(o.line)
		o.done <- err
	}
}

// closedErr says why nothing more can be sent. It is called under mu.
func (s *Session) closedErr() error {
	if s.writeErr != nil {
		return failf(ErrClosed, "the session closed after a write failed: %v", s.writeErr)
	}
	return ErrClosed
}

// observe reads one line the CLI printed for what it says about the messages
// sent into it. Everything else about the line — the answer, the cost, the
// session id — is absorb's business, and readOutput has already done it.
func (s *Session) observe(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return
	}
	// A long run prints thousands of lines and two kinds matter. Decoding
	// every one of them to find out is the one thing this must not do.
	if !bytes.Contains(line, resultKey) && !bytes.Contains(line, controlKey) {
		return
	}
	var e struct {
		Type string `json:"type"`
		// How many accepted messages the CLI has not made turns of yet.
		QueuedTurnCount int `json:"queued_turn_count"`
		Response        struct {
			RequestID string `json:"request_id"`
		} `json:"response"`
	}
	if decodeLenient(line, &e) != nil {
		return
	}
	switch e.Type {
	case "control_response":
		s.emit(Notice{Kind: "interrupted", ID: e.Response.RequestID})
	case "result":
		s.answered(e.QueuedTurnCount)
	}
}

var (
	resultKey  = []byte(`"result"`)
	controlKey = []byte(`"control_response"`)
)

// answered folds one result's queued count into the pending list: everything
// but the last queued messages has had its turn.
func (s *Session) answered(queued int) {
	if queued < 0 {
		queued = 0
	}
	s.mu.Lock()
	n := len(s.pending) - queued
	if n < 0 {
		n = 0
	}
	ids := append([]string(nil), s.pending[:n]...)
	s.pending = append(s.pending[:0], s.pending[n:]...)
	idle := len(s.pending) == 0
	s.mu.Unlock()
	for _, id := range ids {
		s.emit(Notice{Kind: "answered", ID: id})
	}
	if idle {
		s.emit(Notice{Kind: "idle"})
	}
}

// emit offers one notice and never waits. A consumer that has stopped
// draining loses notices; the CLI keeps running, which is the part that
// cannot be undone.
func (s *Session) emit(n Notice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	select {
	case s.notices <- n:
	default:
		s.dropped++
	}
}

// finish stores what the run produced and lets everyone waiting know.
func (s *Session) finish(res *Result, err error) {
	s.mu.Lock()
	s.res, s.err, s.closed, s.finished = res, err, true, true
	s.mu.Unlock()
	close(s.done)
	close(s.notices)
}

// tap passes the CLI's bytes through to the caller's events writer unchanged
// and hands each complete line to the session. The stream a caller sees is
// exactly what the CLI printed; what rota reads out of it changes nothing.
type tap struct {
	out io.Writer
	s   *Session
	// held is the start of a line whose newline has not arrived yet.
	held []byte
	// skip drops the rest of a line too long to hold.
	skip bool
}

// maxHeld bounds what a tap keeps while waiting for a newline. The lines it
// reads are short; a huge one is some other event, passed through and
// forgotten rather than buffered.
const maxHeld = 1 << 20

func (t *tap) Write(p []byte) (int, error) {
	if t.out != nil {
		if _, err := t.out.Write(p); err != nil {
			return 0, err
		}
	}
	rest := p
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break
		}
		if !t.skip {
			line := rest[:i]
			if len(t.held) > 0 {
				t.held = append(t.held, line...)
				line = t.held
			}
			t.s.observe(line)
		}
		t.held, t.skip = t.held[:0], false
		rest = rest[i+1:]
	}
	if !t.skip {
		if len(t.held)+len(rest) > maxHeld {
			t.held, t.skip = t.held[:0], true
		} else {
			t.held = append(t.held, rest...)
		}
	}
	return len(p), nil
}

// Flush forwards to the writer underneath, which is how a streaming
// transport gets each line out as it arrives.
func (t *tap) Flush() {
	if f, ok := t.out.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// The shapes claude's streaming input takes. A message is an envelope around
// content blocks: a plain string for content is refused by the CLI, so the
// blocks are built and marshalled here rather than written out by hand.
type (
	inputMessage struct {
		Type    string       `json:"type"`
		Message inputContent `json:"message"`
	}
	inputContent struct {
		Role    string      `json:"role"`
		Content []inputText `json:"content"`
	}
	inputText struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	controlMessage struct {
		Type      string        `json:"type"`
		RequestID string        `json:"request_id"`
		Request   controlAction `json:"request"`
	}
	controlAction struct {
		Subtype string `json:"subtype"`
	}
)

// messageLine is one user message as a line on the CLI's stdin.
func messageLine(text string) ([]byte, error) {
	return jsonLine(inputMessage{
		Type:    "user",
		Message: inputContent{Role: "user", Content: []inputText{{Type: "text", Text: text}}},
	})
}

// interruptLine is the control request that stops the running tool.
func interruptLine(id string) ([]byte, error) {
	return jsonLine(controlMessage{Type: "control_request", RequestID: id, Request: controlAction{Subtype: "interrupt"}})
}

func jsonLine(v any) ([]byte, error) {
	raw, err := Encode(v)
	if err != nil {
		return nil, failf(ErrInvalidRequest, "cannot encode the message: %v", err)
	}
	return append(raw, '\n'), nil
}
