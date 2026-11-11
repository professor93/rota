package events

import (
	"bufio"
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	rota "github.com/professor93/rota/lib"
)

// A stream of events is server-sent events (text/event-stream): what a
// browser's EventSource reads, and what Reader reads in rota itself.
//
// A stream starts with Retry, then carries one Frame per event, a GapFrame
// first when events it was asked for were not kept, and Heartbeat between
// them while nothing happens.
const (
	// Heartbeat is a comment frame, sent while nothing happens so a proxy
	// or a phone does not take a quiet stream for a dead one.
	Heartbeat = ": ping\n\n"
	// Retry tells an EventSource to reconnect 3 s after the stream drops.
	Retry = "retry: 3000\n\n"
)

// maxLine bounds one line of a stream and the data of one event. An event
// is a few hundred bytes; anything this long is not one.
const maxLine = 64 << 10

// Frame is one event as a stream carries it:
//
//	id: <id>
//	event: <type>
//	data: <the event as one line of JSON>
//
// followed by a blank line, where the data is JSON(e). Frame is always one
// frame: the type goes through Clean first, so a line end in it cannot
// start a frame of its own, and the event: line and the JSON's type are
// always the same string. Clean leaves every real type as it is.
func Frame(e Event) []byte {
	data := JSON(e)
	typ := Clean(e.Type, 0)
	var b bytes.Buffer
	b.Grow(len(data) + len(typ) + 40)
	b.WriteString("id: ")
	b.WriteString(strconv.FormatInt(e.ID, 10))
	b.WriteString("\nevent: ")
	b.WriteString(typ)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// JSON is e as one line of JSON, with no line end: the data of its Frame,
// and what a sink that forwards the event whole sends. Time is written in
// UTC and the type goes through Clean. JSON never fails: a byte that is not
// UTF-8 is written as U+FFFD, a Percent that is not a finite number is left
// out, and a Time RFC 3339 cannot write (a year outside 0 to 9999) is
// written as the zero time.
func JSON(e Event) []byte {
	e.Type = Clean(e.Type, 0)
	e.Time = e.Time.UTC()
	if y := e.Time.Year(); y < 0 || y > 9999 {
		e.Time = time.Time{}
	}
	if e.Percent != nil && (math.IsNaN(*e.Percent) || math.IsInf(*e.Percent, 0)) {
		e.Percent = nil
	}
	data, err := jsonv2.Marshal(e, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		// Unreachable for this struct once the above holds; say so rather
		// than write a frame no reader can take.
		panic("events: an event did not encode: " + err.Error())
	}
	return data
}

// GapFrame is the note a stream sends, once and before any event, when it
// was asked for events older than oldest, the id of the oldest event the
// ring still keeps. A Reader gives it as an Event whose Type is Gap, whose
// Text reads "events before <oldest> were not kept", and whose Time is
// time.UnixMicro(oldest) in UTC: about when the kept history begins, since
// ids follow the clock.
//
// A gap is not a place in the stream. It has no id: line, so a browser's
// Last-Event-ID stays what it was, and its ID is 0. A client that keeps the
// id of the last event it read must not take a gap's.
func GapFrame(oldest int64) []byte {
	return frameNoID(Event{
		Type: Gap,
		Time: time.UnixMicro(oldest).UTC(),
		Text: fmt.Sprintf("events before %d were not kept", oldest),
	})
}

// frameNoID is Frame without its id line.
func frameNoID(e Event) []byte {
	f := Frame(e)
	return f[bytes.IndexByte(f, '\n')+1:]
}

// Reader reads a stream of events. It skips comments, heartbeats and retry
// lines, and refuses a line or an event longer than 64 KiB rather than
// grow without bound.
type Reader struct {
	sc  *bufio.Scanner
	err error
}

// NewReader reads events from r, which is usually an HTTP response's body.
func NewReader(r io.Reader) *Reader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxLine)
	return &Reader{sc: sc}
}

// Next is the stream's next event.
//
// A gap note comes back as an Event whose Type is Gap and whose ID is 0
// (GapFrame): it says events were lost, and must not move the id a client
// keeps as the last it read.
//
// A frame that is not an event comes back as a *FrameError, which names the
// frame by its id: line. That error is not the stream's last: the next call
// goes on to the next frame. A client that keeps a last id moves it forward
// to the FrameError's ID when that is above its own, and never back, so a
// reconnect does not ask for the same frame again.
//
// At the end of the stream Next gives io.EOF, and a frame cut off by the end
// is dropped, as an EventSource drops it. Any other error is the stream's
// last, and every later call gives it again: a read that failed, a line
// longer than 64 KiB, or an event's data longer than that.
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}
	e, err := r.next()
	var fe *FrameError
	if err != nil && !errors.As(err, &fe) {
		r.err = err
	}
	return e, err
}

// FrameError is a frame of the stream that is not an event: its data is not
// JSON, does not fit Event (a daemon of another version that changed a
// field's type, say), or has no type. Reader.Next gives it and goes on.
type FrameError struct {
	// ID is the frame's id: line, 0 when it had none or it was not a number.
	ID int64
	// Err is what was wrong with it.
	Err error
}

func (e *FrameError) Error() string {
	if e.ID > 0 {
		return fmt.Sprintf("events: frame %d is not an event: %v", e.ID, e.Err)
	}
	return "events: a frame that is not an event: " + e.Err.Error()
}

// Unwrap is what was wrong with the frame.
func (e *FrameError) Unwrap() error { return e.Err }

func (r *Reader) next() (Event, error) {
	var data []byte
	var id int64
	have := false // a data line was seen since the last blank line
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			if have && len(data) > 0 {
				return decode(data, id)
			}
			data, id, have = data[:0], 0, false
			continue
		}
		if line[0] == ':' {
			continue // a comment, which is what a heartbeat is
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if have {
				data = append(data, '\n')
			}
			if len(data)+len(value) > maxLine {
				return Event{}, errors.New("events: an event longer than 64 KiB")
			}
			data = append(data, value...)
			have = true
		case "id":
			// The JSON carries the id; this one only names a bad frame.
			// A value that is not an int64, too large ones included, is 0.
			id = 0
			if v, err := strconv.ParseInt(string(value), 10, 64); err == nil {
				id = v
			}
		}
		// event: is the JSON's type again, and retry: is for browsers.
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, errors.New("events: a line longer than 64 KiB")
		}
		return Event{}, err
	}
	return Event{}, io.EOF
}

// decode reads one event's data, the frame whose id: line said id. The
// event is the JSON alone, which must name its type: the event: line is the
// type again for a browser, and is not read.
func decode(data []byte, id int64) (Event, error) {
	var e Event
	if err := jsonv2.Unmarshal(data, &e, rota.LenientOptions()); err != nil {
		return Event{}, &FrameError{ID: id, Err: err}
	}
	if e.Type == "" {
		return Event{}, &FrameError{ID: id, Err: errors.New("it has no type")}
	}
	return e, nil
}
