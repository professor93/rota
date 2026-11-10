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
// followed by a blank line. Time is written in UTC. Frame never fails: a
// byte that is not UTF-8 is written as U+FFFD, a Percent that is not a
// finite number is left out, and a Time RFC 3339 cannot write (a year
// outside 0 to 9999) is written as the zero time.
func Frame(e Event) []byte {
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
	var b bytes.Buffer
	b.Grow(len(data) + len(e.Type) + 40)
	b.WriteString("id: ")
	b.WriteString(strconv.FormatInt(e.ID, 10))
	b.WriteString("\nevent: ")
	b.WriteString(e.Type)
	b.WriteString("\ndata: ")
	b.Write(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

// GapFrame is the note a stream sends, once and before any event, when it
// was asked for events older than the oldest one the ring still keeps. It
// carries no id line, so a browser's Last-Event-ID stays what it was. A
// Reader gives it as Event{Type: Gap} whose Text reads "events before
// <oldest> were not kept".
func GapFrame(oldest int64) []byte {
	return frameNoID(Event{Type: Gap, Text: fmt.Sprintf("events before %d were not kept", oldest)})
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

// Next is the stream's next event. A gap note comes back as an Event whose
// Type is Gap. At the end of the stream Next gives io.EOF, and a frame cut
// off by the end is dropped, as an EventSource drops it. Any other error is
// the stream's last: a read that failed, a line too long, or data that is
// not an event. Every later call gives the same error.
func (r *Reader) Next() (Event, error) {
	if r.err != nil {
		return Event{}, r.err
	}
	e, err := r.next()
	if err != nil {
		r.err = err
	}
	return e, err
}

func (r *Reader) next() (Event, error) {
	var data []byte
	var kind string
	have := false // a data line was seen since the last blank line
	for r.sc.Scan() {
		line := r.sc.Bytes()
		if len(line) == 0 {
			if have && len(data) > 0 {
				return decode(data, kind)
			}
			data, kind, have = data[:0], "", false
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
		case "event":
			kind = string(value)
		}
		// id: is carried in the JSON as well, and retry: is for browsers.
	}
	if err := r.sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return Event{}, errors.New("events: a line longer than 64 KiB")
		}
		return Event{}, err
	}
	return Event{}, io.EOF
}

// decode reads one event's data. The JSON says its type; the event: line is
// used only when the JSON leaves it out.
func decode(data []byte, kind string) (Event, error) {
	var e Event
	if err := jsonv2.Unmarshal(data, &e, rota.LenientOptions()); err != nil {
		return Event{}, fmt.Errorf("events: a frame that is not an event: %w", err)
	}
	if e.Type == "" {
		e.Type = kind
	}
	return e, nil
}
