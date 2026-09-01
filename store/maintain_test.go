package store

import (
	"context"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

func TestMaintainRefreshesTokensAndUsageBeforeAnyoneWaitsOnThem(t *testing.T) {
	quotaCalls := 0
	rota.Register(fakeMeter{fakeRefresher{&fakeProvider{
		name:       "t-keep-meter",
		refreshTok: &rota.Token{Access: "fresh", ExpiresAt: rota.NowMS() + 3_600_000},
		quota:      &rota.Quota{Windows: []rota.Window{win(11, 0, true, false)}},
		quotaCalls: &quotaCalls,
	}}})
	s := openTemp(t)
	// An access token minutes from expiry, and no quota reading at all.
	expiring := &rota.Account{ID: 1, Provider: "t-keep-meter", Order: 1,
		Token: rota.Token{Access: "old", Refresh: "r", ExpiresAt: rota.NowMS() + 1_000}}
	dead := &rota.Account{ID: 2, Provider: "t-keep-meter", Order: 2, Dead: true,
		Token: rota.Token{Access: "x", Refresh: "r"}}
	s.Accounts = []*rota.Account{expiring, dead}

	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if expiring.Token.Access != "fresh" {
		t.Fatalf("a token about to expire must be rotated before a request needs it, got %q", expiring.Token.Access)
	}
	if expiring.Percent() != 11 || expiring.QuotaAt == 0 {
		t.Fatalf("usage must be read too: %v at %d", expiring.Percent(), expiring.QuotaAt)
	}
	if quotaCalls != 1 {
		t.Fatalf("a dead account is not worth a network call: %d", quotaCalls)
	}

	// The cache still governs: a second sweep asks the provider nothing.
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	if quotaCalls != 1 {
		t.Fatalf("a reading younger than the TTL must be reused, got %d calls", quotaCalls)
	}
}

func TestMaintainPersistsWhatItRefreshed(t *testing.T) {
	rota.Register(fakeRefresher{&fakeProvider{
		name:       "t-keep-plain",
		refreshTok: &rota.Token{Access: "fresh", ExpiresAt: rota.NowMS() + int64(time.Hour/time.Millisecond)},
	}})
	b := &memBackend{home: t.TempDir()}
	s, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	s.Accounts = []*rota.Account{{ID: 1, Provider: "t-keep-plain", Order: 1,
		Token: rota.Token{Access: "old", Refresh: "r", ExpiresAt: rota.NowMS() + 1_000}}}
	if errs := s.Maintain(context.Background()); len(errs) != 0 {
		t.Fatalf("%v", errs)
	}
	s.Close()

	again, err := NewStore(b)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	// A rotated token that is not on disk is a token lost, whether it was a
	// run or a background sweep that rotated it.
	if got := again.Find(1).Token.Access; got != "fresh" {
		t.Fatalf("got %q", got)
	}
}
