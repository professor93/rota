package message

import (
	"bytes"
	"encoding/json/jsontext"
	"time"
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

	// With is what to carry on each event beyond the event: the provider's
	// own line (Raw), the split of a whole text (Blocks), the time it
	// arrived (Timing). The rest of With is read from a finished answer.
	With With
	// Tally, when given, is told every event, for the readings that need
	// to have seen the whole stream: files, tools, stats, timing.
	Tally *Tally

	Emit func(Event) error

	seq   int
	buf   []byte
	start time.Time // when the first event was sent; at is measured from it
}

// Write takes whatever the CLI printed and turns the whole lines in it into
// events.
//
// A writer may be handed half a line, and half a line is half an event, so
// the remainder is held until its newline arrives. What is left when the run
// ends belongs to Rest.
func (s *Stream) Write(p []byte) (int, error) {
	if s.Tally != nil {
		s.Tally.Bytes += int64(len(p))
	}
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
	// Time is measured from the first event, which is rota's own init when
	// there is one: at is how long after the run was announced this came.
	now := time.Now()
	if s.start.IsZero() {
		s.start = now
	}
	at := now.Sub(s.start)
	if s.With.Timing {
		ev.At = at.Milliseconds()
	}
	if s.Tally != nil {
		s.Tally.add(ev, at)
	}
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
		if s.With.Raw {
			// A copy, because the buffer under it is reused by the next read.
			ev.Raw = jsontext.Value(bytes.Clone(bytes.TrimRight(line, "\r")))
		}
		// A whole piece of text is split when asked; a fragment never is,
		// since half a fence is not a fence.
		if s.With.Blocks && ev.Type == "text" && !ev.Delta && ev.Text != "" {
			ev.Blocks = Blocks(ev.Text)
		}
		if err := s.Send(ev); err != nil {
			return err
		}
	}
	return nil
}
