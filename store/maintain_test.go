package store

import (
	"context"
	"errors"
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

// adoptsAndRefreshes stands in for a CLI that keeps its login in a home rota
// reads back before it refreshes: adopt is what that reading finds, by
// account id, and a refresh always answers with the token "new".
type adoptsAndRefreshes struct {
	*fakeProvider
	adopt map[int]string
}

func (p adoptsAndRefreshes) Adopt(a *rota.Account, _ string) error {
	if access, ok := p.adopt[a.ID]; ok {
		a.Token.Access = access
	}
	return nil
}

func (adoptsAndRefreshes) Refresh(context.Context, *rota.Account) (*rota.Token, error) {
	return &rota.Token{Access: "new", Refresh: "r-new", ExpiresAt: rota.NowMS() + 3_600_000}, nil
}

// failingSaves is a memBackend whose next fail saves are refused.
type failingSaves struct {
	*memBackend
	fail *int
}

func (f failingSaves) Save(b []byte) error {
	if *f.fail > 0 {
		*f.fail--
		return errors.New("the disk is full")
	}
	return f.memBackend.Save(b)
}

// A sweep saves a token it refreshed at once, so nothing can stand between
// the spent one and the store, and it saves again at its end only when
// something changed after that save: one save carries every change made
// before it, whichever account made it. A save that fails settles nothing, so
// the one at the end tries again.
func TestASweepSavesOnlyWhatIsStillUnsaved(t *testing.T) {
	const provider = "t-adopts-refreshes"
	due := func(id int) *rota.Account {
		return &rota.Account{ID: id, Provider: provider, Order: id,
			Token: rota.Token{Access: "old", Refresh: "r", ExpiresAt: 1}}
	}
	fresh := func(id int) *rota.Account {
		return &rota.Account{ID: id, Provider: provider, Order: id,
			Token: rota.Token{Access: "good", Refresh: "r", ExpiresAt: rota.NowMS() + 3_600_000}}
	}
	for _, c := range []struct {
		what      string
		accounts  []*rota.Account
		adopt     map[int]string // what each account's home is found to hold
		failSaves int
		errs      int
		saves     int
		want      map[int]string // each account's access token on disk afterwards
	}{
		{what: "one account, adopted then refreshed",
			accounts: []*rota.Account{due(1)}, saves: 1,
			want: map[int]string{1: "new"}},
		{what: "two accounts, only the first refreshed",
			accounts: []*rota.Account{due(1), fresh(2)}, saves: 1,
			want: map[int]string{1: "new", 2: "good"}},
		{what: "an adoption before the refresh goes with its save",
			accounts: []*rota.Account{fresh(1), due(2)}, adopt: map[int]string{1: "adopted"}, saves: 1,
			want: map[int]string{1: "adopted", 2: "new"}},
		{what: "an adoption after the refresh is saved at the end",
			accounts: []*rota.Account{due(1), fresh(2)}, adopt: map[int]string{2: "adopted"}, saves: 2,
			want: map[int]string{1: "new", 2: "adopted"}},
		{what: "nothing changed",
			accounts: []*rota.Account{fresh(1), fresh(2)}, saves: 0,
			want: map[int]string{1: "good", 2: "good"}},
		{what: "a refreshed token whose save failed is saved at the end",
			accounts: []*rota.Account{due(1), fresh(2)}, failSaves: 1, errs: 1, saves: 1,
			want: map[int]string{1: "new", 2: "good"}},
	} {
		rota.Register(adoptsAndRefreshes{fakeProvider: &fakeProvider{name: provider}, adopt: c.adopt})
		m := &memBackend{home: t.TempDir()}
		fail := 0
		b := failingSaves{memBackend: m, fail: &fail}
		s, err := NewStore(b)
		if err != nil {
			t.Fatal(err)
		}
		s.Accounts = c.accounts
		fail = c.failSaves
		if errs := s.Maintain(context.Background()); len(errs) != c.errs {
			t.Fatalf("%s: errors %v, want %d", c.what, errs, c.errs)
		}
		s.Close()
		if m.saves != c.saves {
			t.Fatalf("%s: the sweep saved %d times, want %d", c.what, m.saves, c.saves)
		}
		if c.saves == 0 {
			continue // nothing on disk to read back
		}
		again, err := NewStore(m)
		if err != nil {
			t.Fatal(err)
		}
		for id, access := range c.want {
			if got := again.Find(id).Token.Access; got != access {
				t.Fatalf("%s: account %d holds %q on disk, want %q", c.what, id, got, access)
			}
		}
		again.Close()
	}
}
