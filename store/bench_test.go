package store

import (
	"path/filepath"
	"testing"
)

// BenchmarkOpen is what every HTTP request pays before it does anything:
// take the lock, read the file, parse it. If this were slow, a busy server
// would feel it on every call.
func BenchmarkOpen(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir)
	if err != nil {
		b.Fatal(err)
	}
	for range 20 {
		a := s.add("claude")
		a.Email = "someone@example.com"
		a.Token.Access = "a-token-of-plausible-length-0123456789"
	}
	if err := s.Save(); err != nil {
		b.Fatal(err)
	}
	s.Close()
	b.ReportAllocs()
	for b.Loop() {
		st, err := Open(dir)
		if err != nil {
			b.Fatal(err)
		}
		st.Close()
	}
	_ = filepath.Join(dir, "accounts.json")
}

// BenchmarkSave is the write side, atomic rename included.
func BenchmarkSave(b *testing.B) {
	s, err := Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	for range 20 {
		s.add("claude").Token.Access = "tok"
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := s.Save(); err != nil {
			b.Fatal(err)
		}
	}
}
