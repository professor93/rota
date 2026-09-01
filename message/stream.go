package message

import (
	"bytes"
	"encoding/json/jsontext"
)

// Stream turns the lines a vendor CLI prints into rota's own events.
//
// It exists once so that every transport says the same thing. The CLI and the
// HTTP API both stream a run, and they used to split, normalize and number the
// events separately: two implementations of one thing, which agree until the
// day one of them is edited. What differs between them is only how a finished
// event is framed — Server-Sent Events, newline-delimited JSON, or prose — and
// that is all either of them keeps now.
//
// Emit receives each event, stamped. A nil Emit drops them, which is what a
// caller that only wants the buffered result at the end wants.
type Stream struct {
	Account  int
	Provider string

	// Raw carries the provider's own line along on each event. It is much
	// the largest part of one, so it is sent only when asked for.
	Raw bool

	Emit func(Event) error

	seq int
	buf []byte
}

// Write takes whatever the CLI printed and turns the whole lines in it into
// events.
//
// A writer may be handed half a line, and half a line is half an event, so
// the remainder is held until its newline arrives. What is left when the run
// ends belongs to Rest.
func (s *Stream) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := s.buf[:i]
		s.buf = s.buf[i+1:]
		if err := s.line(line); err != nil {
			return 0, err
		}
	}
}

// Rest sends anything the CLI wrote without a closing newline. A last event
// with no byte after it is still an event.
func (s *Stream) Rest() error {
	line := s.buf
	s.buf = nil
	return s.line(line)
}

// Seq is how many events have been sent, which is the number the next one
// will carry.
func (s *Stream) Seq() int { return s.seq }

// Send stamps one event of rota's own — the opening one, saying what the run
// is about to do — and emits it.
func (s *Stream) Send(ev Event) error {
	s.seq++
	ev.Seq, ev.Account, ev.Provider = s.seq, s.Account, s.Provider
	if s.Emit == nil {
		return nil
	}
	return s.Emit(ev)
}

func (s *Stream) line(line []byte) error {
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	for _, ev := range Normalize(line) {
		if s.Raw {
			// A copy, because the buffer under it is reused by the next read.
			ev.Raw = jsontext.Value(bytes.Clone(bytes.TrimRight(line, "\r")))
		}
		if err := s.Send(ev); err != nil {
			return err
		}
	}
	return nil
}
