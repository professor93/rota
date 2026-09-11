package main

import (
	"fmt"
	"io"
	"strings"
	"sync"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/message"
)

// eventStream renders a run's events as they arrive.
//
// The reading of them is message.Stream's, shared with the HTTP API so both
// transports cannot drift into saying different things. What is left here is
// only how a finished event is written, and that depends on what was asked
// for: in text mode the prose and nothing else, because someone who asked for
// an answer should not get a screenful of the provider's own JSON; in JSON
// mode newline-delimited JSON, exactly one complete object per line.
type eventStream struct {
	out  io.Writer
	json bool

	// mu guards the stream and the writer under it. A run that takes more
	// messages has two sources: the CLI's output, read on one goroutine, and
	// what became of each message sent in, read on another. They would
	// otherwise interleave halves of a line and race over the sequence
	// numbers. It is held across message.Stream's own calls, which is why
	// Stream needs no lock of its own: everything that reaches it comes
	// through one of the three doors below.
	mu sync.Mutex

	stream message.Stream
	text   bool // whether any prose has been printed, so a newline can close it

	// shown is what has been printed as fragments and not yet arrived whole.
	// A whole piece that begins it has been seen already, and is skipped.
	shown string

	// quiet reads the events without printing any of them. A run that was not
	// asked to stream still has something worth watching go past: the
	// conversation id, which the CLI decides and rota cannot know until it is
	// said. Reading it here costs one pass that is already being made — though
	// a buffered CLI says it only at the end, when the run is nearly over.
	quiet bool

	// learn is told the conversation this run turned out to be in, once.
	learn func(string)
}

// with names what --with asked to carry on each event; tally, when given,
// is told every event for the readings that need the whole stream.
func newEventStream(out io.Writer, asJSON bool, account int, provider string, with message.With, tally *message.Tally) *eventStream {
	e := &eventStream{out: out, json: asJSON}
	e.stream = message.Stream{Account: account, Provider: provider, With: with, Tally: tally, Emit: e.send}
	return e
}

// Write is the first door: whatever the CLI printed, turned into events.
func (e *eventStream) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stream.Write(p)
}

// emit is the second: an event of rota's own — the opening one, and what
// became of each message sent into an open run.
func (e *eventStream) emit(ev message.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stream.Send(ev)
}

// send writes one finished event in whichever form was asked for. Every path
// to it holds mu already: it is message.Stream's Emit, and the stream is only
// ever entered through the three doors that take the lock.
func (e *eventStream) send(ev message.Event) error {
	if ev.SessionID != "" && e.learn != nil {
		e.learn(ev.SessionID)
	}
	if e.quiet {
		return nil
	}
	if e.json {
		return rota.EncodeTo(e.out, ev)
	}
	if ev.Type != "text" || ev.Text == "" {
		return nil
	}
	switch rest, seen := strings.CutPrefix(e.shown, ev.Text); {
	case ev.Delta:
		e.shown += ev.Text
	case seen:
		// The whole of what the fragments already showed: nothing new to
		// print, only to stop waiting for it.
		e.shown = rest
		return nil
	default:
		// A whole piece the fragments did not add up to. Printing it may
		// repeat some of them, which is better than losing any of it, and
		// what was pending is not coming.
		e.shown = ""
	}
	if _, err := fmt.Fprint(e.out, ev.Text); err != nil {
		return err
	}
	e.text = true
	return nil
}

// end writes the terminal event and closes the stream off.
//
// It is the same shape the HTTP surface ends with, so a reader that knows one
// knows the other. In text mode there is nothing left to say — the answer has
// been printed — beyond finishing the line, since a CLI that streamed its
// answer in pieces need not have ended on one.
func (e *eventStream) end(end any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.stream.Rest() // a last line with no newline after it is still an event
	if e.json {
		_ = rota.EncodeTo(e.out, end)
		return
	}
	if e.text {
		fmt.Fprintln(e.out)
	}
}
