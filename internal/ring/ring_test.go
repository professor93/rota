package ring

import (
	"bytes"
	"testing"
)

// A ring made to continue somebody else's offsets answers in them: a daemon
// that links to a holder mid-session keeps the holder's numbering rather
// than starting its own at zero.
func TestRingStartsWhereItIsTold(t *testing.T) {
	r := NewAt(8, 100)
	if r.Oldest() != 100 || r.End() != 100 {
		t.Fatalf("an empty ring at 100: oldest %d, end %d", r.Oldest(), r.End())
	}
	r.Write([]byte("abcd"))
	if r.Oldest() != 100 || r.End() != 104 {
		t.Fatalf("oldest %d, end %d, want 100 and 104", r.Oldest(), r.End())
	}
	data, at := r.Read(50)
	if at != 100 || string(data) != "abcd" {
		t.Fatalf("Read(50) = %q at %d, want %q at 100", data, at, "abcd")
	}
}

// Past its size a ring keeps the newest bytes, across the seam where it
// wraps, and a burst larger than the whole of it leaves only its tail.
func TestRingKeepsTheNewestAcrossTheSeam(t *testing.T) {
	r := New(8)
	r.Write([]byte("012345"))
	r.Write([]byte("6789"))
	if r.Oldest() != 2 || r.End() != 10 {
		t.Fatalf("oldest %d, end %d, want 2 and 10", r.Oldest(), r.End())
	}
	if data, at := r.Read(0); at != 2 || string(data) != "23456789" {
		t.Fatalf("Read(0) = %q at %d", data, at)
	}
	if data, at := r.Read(7); at != 7 || string(data) != "789" {
		t.Fatalf("Read(7) = %q at %d", data, at)
	}
	if data, at := r.Read(99); at != 10 || len(data) != 0 {
		t.Fatalf("a read past the end is empty at the end: %q at %d", data, at)
	}
	r.Write(bytes.Repeat([]byte("x"), 20))
	if data, at := r.Read(0); at != 22 || string(data) != "xxxxxxxx" {
		t.Fatalf("after a burst: %q at %d", data, at)
	}
}

// Told nothing about its size, a ring keeps the default rather than none.
func TestRingWithoutASizeKeepsTheDefault(t *testing.T) {
	r := New(0)
	r.Write([]byte("hello"))
	if data, _ := r.Read(0); string(data) != "hello" {
		t.Fatalf("%q", data)
	}
	if len(r.buf) != DefaultKeep {
		t.Fatalf("kept %d, want %d", len(r.buf), DefaultKeep)
	}
}
