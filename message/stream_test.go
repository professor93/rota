package message

import (
	"strings"
	"testing"
)

// collect is a sink that keeps what a stream sent.
func collect(s *Stream) *[]Event {
	var got []Event
	s.Emit = func(ev Event) error {
		got = append(got, ev)
		return nil
	}
	return &got
}

// A stream stamps every event with where it came from and its place in the
// sequence. The number is a reader's only way to notice one it never got.
func TestAStreamStampsEveryEventInOrder(t *testing.T) {
	s := &Stream{Account: 2, Provider: "claude"}
	got := collect(s)

	s.Send(Event{Type: "init", Model: "claude-opus-5"})
	s.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n"))

	if len(*got) != 2 {
		t.Fatalf("two events, got %d: %+v", len(*got), *got)
	}
	for i, ev := range *got {
		if ev.Seq != i+1 {
			t.Fatalf("event %d is numbered %d", i, ev.Seq)
		}
		if ev.Account != 2 || ev.Provider != "claude" {
			t.Fatalf("every event says whose run it is: %+v", ev)
		}
	}
}

// A writer may be handed half a line, and half a line is half an event. The
// remainder waits for its newline rather than being sent as a broken one.
func TestAPartialLineWaitsForItsNewline(t *testing.T) {
	s := &Stream{}
	got := collect(s)

	s.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text",`))
	if len(*got) != 0 {
		t.Fatalf("nothing is complete yet: %+v", *got)
	}
	s.Write([]byte(`"text":"hi"}]}}` + "\n"))
	if len(*got) != 1 || (*got)[0].Text != "hi" {
		t.Fatalf("the halves make one event: %+v", *got)
	}
}

// Several lines in one write are several events, and a last line with no
// newline after it is still an event: it is sent when the run is over rather
// than dropped for want of a byte.
func TestManyLinesAtOnceAndALastLineWithNoNewline(t *testing.T) {
	s := &Stream{}
	got := collect(s)

	s.Write([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"three"}]}}`))
	if len(*got) != 2 {
		t.Fatalf("two whole lines so far: %+v", *got)
	}
	s.Rest()
	if len(*got) != 3 || (*got)[2].Text != "three" {
		t.Fatalf("the unterminated last line is still an event: %+v", *got)
	}
	// And calling it again sends nothing twice.
	s.Rest()
	if len(*got) != 3 {
		t.Fatalf("%+v", *got)
	}
}

// Blank lines are not events, and neither is trailing whitespace.
func TestBlankLinesAreNotEvents(t *testing.T) {
	s := &Stream{}
	got := collect(s)
	s.Write([]byte("\n\n   \n"))
	s.Rest()
	if len(*got) != 0 {
		t.Fatalf("%+v", *got)
	}
}

// The provider's own line rides along only when it was asked for, because it
// is much the largest thing in an event and most readers want none of it.
func TestTheProvidersOwnLineRidesAlongOnlyWhenAsked(t *testing.T) {
	const line = `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`
	s := &Stream{}
	got := collect(s)
	s.Write([]byte(line + "\n"))
	if len((*got)[0].Raw) != 0 {
		t.Fatalf("not by default: %s", (*got)[0].Raw)
	}

	s = &Stream{Raw: true}
	got = collect(s)
	s.Write([]byte(line + "\n"))
	if !strings.Contains(string((*got)[0].Raw), `"assistant"`) {
		t.Fatalf("asked for, and it is the line as it arrived: %s", (*got)[0].Raw)
	}
}
