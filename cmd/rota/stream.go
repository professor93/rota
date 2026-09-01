package main

import (
	"fmt"
	"io"

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

	stream message.Stream
	text   bool // whether any prose has been printed, so a newline can close it

	// quiet reads the events without printing any of them. A run that was not
	// asked to stream still has something worth watching go past: the
	// conversation id, which the CLI decides and rota cannot know until it is
	// said. Reading it here costs one pass that is already being made — though
	// a buffered CLI says it only at the end, when the run is nearly over.
	quiet bool

	// learn is told the conversation this run turned out to be in, once.
	learn func(string)
}

func newEventStream(out io.Writer, asJSON bool, account int, provider string) *eventStream {
	e := &eventStream{out: out, json: asJSON}
	e.stream = message.Stream{Account: account, Provider: provider, Emit: e.send}
	return e
}

func (e *eventStream) Write(p []byte) (int, error) { return e.stream.Write(p) }

// send writes one finished event in whichever form was asked for.
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
	if ev.Type == "text" && ev.Text != "" {
		if _, err := fmt.Fprint(e.out, ev.Text); err != nil {
			return err
		}
		e.text = true
	}
	return nil
}

// end writes the terminal event and closes the stream off.
//
// It is the same shape the HTTP surface ends with, so a reader that knows one
// knows the other. In text mode there is nothing left to say — the answer has
// been printed — beyond finishing the line, since a CLI that streamed its
// answer in pieces need not have ended on one.
func (e *eventStream) end(end any) {
	_ = e.stream.Rest() // a last line with no newline after it is still an event
	if e.json {
		_ = rota.EncodeTo(e.out, end)
		return
	}
	if e.text {
		fmt.Fprintln(e.out)
	}
}
