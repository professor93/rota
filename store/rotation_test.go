package store

import (
	"encoding/json"
	"testing"

	rota "github.com/professor93/rota/lib"
)

// loadRaw builds a store over bytes a previous version could have written.
func loadRaw(t *testing.T, blob string) *Store {
	t.Helper()
	s, err := NewStore(&memBackend{blob: []byte(blob), home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Numbering a new account is bookkeeping, like allocating its id: what
// the number means is the rotation package's business, but a store that
// left every new account at 0 would quietly keep it out of any queue.
func TestNewAccountsJoinTheEndOfTheQueue(t *testing.T) {
	s := loadRaw(t, `{"ordered":true,"accounts":[{"id":1,"provider":"claude","order":4}]}`)
	a := s.add("claude")
	if a.Order != 5 {
		t.Fatalf("got order %d, want one past the last in the queue", a.Order)
	}
	if b := s.add("codex"); b.Order != 6 {
		t.Fatalf("got order %d", b.Order)
	}
}

func TestOrderAndThresholdSurviveASaveAndLoad(t *testing.T) {
	b := &memBackend{home: t.TempDir()}
	s, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	s.Accounts = []*rota.Account{{ID: 1, Provider: "claude", Order: 2, Threshold: 75}}
	s.Ordered = true
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s.Close()

	again, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	a := again.Find(1)
	if a.Order != 2 || a.Threshold != 75 {
		t.Fatalf("got order %d threshold %d", a.Order, a.Threshold)
	}
	var doc map[string]any
	if err := json.Unmarshal(b.blob, &doc); err != nil {
		t.Fatal(err)
	}
}
