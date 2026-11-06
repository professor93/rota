package holdwire

import (
	"bytes"
	"testing"

	"github.com/professor93/rota/internal/vtscan"
	rota "github.com/professor93/rota/lib"
)

// Each raw payload keeps its number and its bytes, and a Screen keeps its
// header apart from a repaint that holds the very bytes a naive split would
// cut it at.
func TestFramesRoundTrip(t *testing.T) {
	out := PutOutput(1<<40+7, []byte("hello\x00\n"))
	off, p, err := OutputOf(out)
	if err != nil || off != 1<<40+7 || string(p) != "hello\x00\n" {
		t.Fatalf("OutputOf = %d %q %v", off, p, err)
	}
	if off, p, err := OutputOf(PutOutput(0, nil)); err != nil || off != 0 || len(p) != 0 {
		t.Fatalf("empty OutputOf = %d %q %v", off, p, err)
	}
	if _, _, err := OutputOf([]byte{1, 2, 3}); err == nil {
		t.Fatal("a payload shorter than its offset was read")
	}

	in := PutInput(42, []byte("ls\r"))
	seq, p, err := InputOf(in)
	if err != nil || seq != 42 || string(p) != "ls\r" {
		t.Fatalf("InputOf = %d %q %v", seq, p, err)
	}
	if _, _, err := InputOf(nil); err == nil {
		t.Fatal("an empty Input was read")
	}

	modes := vtscan.Modes{AltScreen: true, BracketedPaste: true, Mouse: 1002, MouseEncoding: 1006, Kitty: 5}
	head := ScreenHead{Seq: 9, OK: true, Offset: 12345, Cols: 120, Rows: 40, Modes: modes}
	repaint := []byte("\x1b[H\x1b[2Jline one\nline\x00two\n")
	raw, err := PutScreen(head, repaint)
	if err != nil {
		t.Fatal(err)
	}
	got, body, err := ScreenOf(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got != head {
		t.Fatalf("the header came back as %+v, want %+v", got, head)
	}
	if !bytes.Equal(body, repaint) {
		t.Fatalf("the repaint came back as %q", body)
	}
	// The modes travel in State too, beside what the CLI's registry says.
	state := StateMsg{Title: "work", Bells: 2, Modes: modes, Registry: &Registry{SessionID: "s", WaitingFor: "permission"}}
	raw, err = rota.Encode(state)
	if err != nil {
		t.Fatal(err)
	}
	var back StateMsg
	if err := rota.UnmarshalLenient(raw, &back); err != nil || back.Modes != modes || back.Registry == nil ||
		*back.Registry != *state.Registry || back.Title != "work" || back.Bells != 2 {
		t.Fatalf("State came back as %+v (%v) from %s", back, err, raw)
	}
	if _, _, err := ScreenOf([]byte(`{"seq":1}`)); err == nil {
		t.Fatal("a Screen with no end to its header was read")
	}
	if _, _, err := ScreenOf([]byte("not json\nrest")); err == nil {
		t.Fatal("a Screen whose header is not JSON was read")
	}
}

// Both ends say which majors they speak, and the link runs on the highest
// they share or not at all.
func TestAgreePicksTheHighestCommonMajor(t *testing.T) {
	if m, ok := Agree([]int{1}, []int{1, 2}); !ok || m != 1 {
		t.Fatalf("({1},{1,2}) = %d %v", m, ok)
	}
	if _, ok := Agree([]int{2}, []int{1}); ok {
		t.Fatal("({2},{1}) agreed")
	}
	if m, ok := Agree([]int{1, 3, 2}, []int{3, 2}); !ok || m != 3 {
		t.Fatalf("({1,3,2},{3,2}) = %d %v", m, ok)
	}
	if _, ok := Agree(nil, []int{1}); ok {
		t.Fatal("nothing agreed with something")
	}
	if m, ok := Agree(Majors, []int{Major}); !ok || m != Major {
		t.Fatalf("this rota does not agree with itself: %d %v", m, ok)
	}
}
