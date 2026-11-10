package events

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// full is an Event with every field set, whose text needs escaping.
func full() Event {
	exit, pct := 129, 82.5
	return Event{
		ID:      1767225600000001,
		Type:    SessionEnded,
		Time:    time.Date(2026, 11, 10, 21, 20, 5, 123456000, time.UTC),
		Account: 3,
		Session: "s-1",
		Name:    "refactor",
		Text:    "said \"hi\"\nand left",
		Reason:  "stopped",
		Exit:    &exit,
		Window:  "5h",
		Percent: &pct,
		Limit:   80,
		Viewer:  "alice",
		Sink:    "telegram",
	}
}

func TestFrameRoundTrips(t *testing.T) {
	e := full()
	f := Frame(e)
	head := fmt.Sprintf("id: %d\nevent: %s\ndata: ", e.ID, e.Type)
	if !bytes.HasPrefix(f, []byte(head)) || !bytes.HasSuffix(f, []byte("}\n\n")) {
		t.Fatalf("Frame = %q", f)
	}
	if n := bytes.Count(f, []byte("\n")); n != 4 {
		t.Fatalf("Frame has %d newlines, want 4: %q", n, f)
	}
	got, err := NewReader(bytes.NewReader(f)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if !got.Time.Equal(e.Time) || got.Time.Location() != time.UTC {
		t.Fatalf("time came back as %v", got.Time)
	}
	got.Time = e.Time
	if *got.Exit != *e.Exit || *got.Percent != *e.Percent {
		t.Fatalf("exit %d, percent %v", *got.Exit, *got.Percent)
	}
	got.Exit, got.Percent = e.Exit, e.Percent
	if got != e {
		t.Fatalf("came back as %+v, want %+v", got, e)
	}
}

func TestFrameKeysAndUTC(t *testing.T) {
	zone := time.FixedZone("x", 5*3600)
	e := Event{ID: 9, Type: Test, Time: time.Date(2026, 11, 10, 2, 0, 0, 0, zone), Text: "A test event from local"}
	data := strings.TrimSuffix(strings.SplitN(string(Frame(e)), "data: ", 2)[1], "\n\n")
	want := `{"id":9,"type":"test","time":"2026-11-09T21:00:00Z","account":0,"session":"","name":"","text":"A test event from local"}`
	if data != want {
		t.Fatalf("data = %s\nwant   %s", data, want)
	}
	zero, pct := 0, 0.0
	e.Exit, e.Percent = &zero, &pct
	if d := string(Frame(e)); !strings.Contains(d, `"exit":0`) || !strings.Contains(d, `"percent":0`) {
		t.Fatalf("a zero exit or percent was left out: %s", d)
	}
}

func TestFrameNeverFails(t *testing.T) {
	nan := 0.0
	nan /= nan
	e := Event{ID: 1, Type: AccountThreshold, Text: "bad \xff byte", Percent: &nan,
		Time: time.Date(12000, 1, 1, 0, 0, 0, 0, time.UTC)}
	got, err := NewReader(bytes.NewReader(Frame(e))).Next()
	if err != nil {
		t.Fatalf("Frame of an odd event did not read back: %v (%q)", err, Frame(e))
	}
	if got.Type != AccountThreshold || got.Percent != nil || !got.Time.IsZero() || got.Text != "bad \ufffd byte" {
		t.Fatalf("read back %+v", got)
	}
}

func TestReaderSkipsWhatIsNotAnEvent(t *testing.T) {
	var in bytes.Buffer
	in.WriteString(Retry)
	in.WriteString(Heartbeat)
	in.WriteString(": anything\n\n")
	in.WriteString("\n")
	in.Write(GapFrame(7))
	r := NewReader(&in)
	got, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	want := Event{Type: Gap, Time: time.UnixMicro(7).UTC(), Text: "events before 7 were not kept"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("after the gap, %v, want io.EOF", err)
	}
}

func TestGapFrameHasNoID(t *testing.T) {
	if f := string(GapFrame(7)); strings.Contains(f, "id: ") || !strings.HasPrefix(f, "event: gap\ndata: ") {
		t.Fatalf("GapFrame = %q", f)
	}
}

func TestReaderJoinsDataAndTakesCRLF(t *testing.T) {
	in := "id: 5\r\nevent: test\r\ndata: {\"id\":5,\r\ndata: \"type\":\"test\"}\r\n\r\n"
	got, err := NewReader(strings.NewReader(in)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 5 || got.Type != Test {
		t.Fatalf("got %+v", got)
	}
}

func TestReaderReadsInOrder(t *testing.T) {
	var in bytes.Buffer
	for i := int64(1); i <= 3; i++ {
		in.Write(Frame(Event{ID: i, Type: Test}))
		in.WriteString(Heartbeat)
	}
	in.WriteString("data: {\"id\":4,\"type\":\"test\"}") // cut off mid-frame
	r := NewReader(&in)
	for i := int64(1); i <= 3; i++ {
		e, err := r.Next()
		if err != nil || e.ID != i {
			t.Fatalf("event %d: %+v %v", i, e, err)
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("a frame cut off is %v, want io.EOF", err)
	}
}

func TestReaderSkipsEmptyData(t *testing.T) {
	r := NewReader(strings.NewReader("data:\n\nevent: test\n\n" + string(Frame(Event{ID: 2, Type: Test}))))
	if e, err := r.Next(); err != nil || e.ID != 2 {
		t.Fatalf("got %+v %v", e, err)
	}
}

func TestABadFrameIsSkippedAndNamed(t *testing.T) {
	in := "id: 5\ndata: {nope\n\n" +
		"id: 6\nevent: account.reset\ndata: {\"id\":6,\"type\":\"account.reset\",\"account\":\"three\"}\n\n" +
		"id: 7\ndata: {}\n\n" +
		"data: null\n\n" +
		"id: x\ndata: {\"id\":8,\"text\":\"no type\"}\n\n" +
		string(Frame(Event{ID: 9, Type: Test}))
	r := NewReader(strings.NewReader(in))
	for _, id := range []int64{5, 6, 7, 0, 0} {
		_, err := r.Next()
		var fe *FrameError
		if !errors.As(err, &fe) || fe.ID != id {
			t.Fatalf("frame %d gave %v, want a FrameError for it", id, err)
		}
		if id > 0 && !strings.Contains(err.Error(), fmt.Sprint(id)) {
			t.Fatalf("the error does not name frame %d: %v", id, err)
		}
	}
	if e, err := r.Next(); err != nil || e.ID != 9 {
		t.Fatalf("after the bad frames, %+v %v, want event 9", e, err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("at the end, %v, want io.EOF", err)
	}
}

func TestAFrameCannotForgeAnother(t *testing.T) {
	forged := "test\r\ndata: {\"type\":\"daemon.stopped\"}\n\nevent: evil\rid: 99\n"
	e := Event{ID: 3, Type: forged, Text: "x"}
	f := Frame(e)
	if n := bytes.Count(f, []byte("\n")); n != 4 || bytes.ContainsRune(f, '\r') {
		t.Fatalf("Frame of a type with line ends = %q", f)
	}
	r := NewReader(bytes.NewReader(f))
	got, err := r.Next()
	if err != nil || got.ID != 3 || got.Type != forged {
		t.Fatalf("read back %+v %v", got, err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("a second frame came out: %v", err)
	}
}

func TestReaderBoundsALine(t *testing.T) {
	big := "data: " + strings.Repeat("x", 70<<10) + "\n\n"
	r := NewReader(strings.NewReader(big))
	done := make(chan error, 1)
	go func() { _, err := r.Next(); done <- err }()
	select {
	case err := <-done:
		if err == nil || err == io.EOF {
			t.Fatalf("a 70 KiB line gave %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a 70 KiB line hung the reader")
	}
	if _, err := r.Next(); err == nil || err == io.EOF {
		t.Fatalf("after the error, %v", err)
	}
}

func TestReaderBoundsAnEvent(t *testing.T) {
	// Valid JSON over two lines, each under the line bound, together over it.
	x := strings.Repeat("x", 40<<10)
	in := "data: {\"id\":1,\"type\":\"test\",\"text\":\"" + x + "\"\ndata: ,\"name\":\"" + x + "\"}\n\n"
	r := NewReader(strings.NewReader(in))
	if _, err := r.Next(); err == nil || err == io.EOF {
		t.Fatalf("an 80 KiB event gave %v", err)
	}
}

func TestReaderPassesOnAReadError(t *testing.T) {
	boom := errors.New("boom")
	r := NewReader(io.MultiReader(strings.NewReader(Heartbeat), errReader{boom}))
	if _, err := r.Next(); !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func FuzzReader(f *testing.F) {
	f.Add(Frame(full()))
	f.Add(GapFrame(7))
	f.Add([]byte(Retry + Heartbeat + ": anything\n\n\n"))
	f.Add([]byte("id: 5\r\nevent: test\r\ndata: {\"id\":5,\r\ndata: \"type\":\"test\"}\r\n\r\n"))
	f.Add([]byte("data: {nope\n\n"))
	f.Add([]byte("data\ndata:\n\nevent\n:\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		r := NewReader(bytes.NewReader(in))
		for i := 0; i <= len(in)+1; i++ {
			var fe *FrameError
			if _, err := r.Next(); errors.As(err, &fe) {
				continue
			} else if err != nil {
				return
			}
		}
		t.Fatalf("Next gave more events than %d bytes can hold", len(in))
	})
}
