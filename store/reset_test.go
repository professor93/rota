package store

import (
	"context"
	"strings"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
)

// clockAt points the clock rota reads at *now for the rest of the test, so a
// test moves time by assigning to it rather than by waiting.
func clockAt(t *testing.T, now *time.Time) {
	t.Helper()
	old := rota.Now
	rota.Now = func() time.Time { return *now }
	t.Cleanup(func() { rota.Now = old })
}

func spentUntil(pct float64, reset time.Time) *rota.Quota {
	return &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: pct, Primary: true, ResetsAt: rota.When{Time: reset}}}}
}

// A reading taken before a window it reports resets says nothing true about
// that window afterwards: an account spent until five is whole again at five,
// and the reading that still says spent would keep the rotation passing it
// by until the cache ran out. So it is read again once the reset has passed,
// cache or no cache — and only once: a reading taken after the reset is fresh
// whatever reset time it reports.
func TestAReadingIsReadAgainOnceAWindowItReportedHasReset(t *testing.T) {
	now := time.Now()
	clockAt(t, &now)
	calls := 0
	p := &fakeProvider{name: "t-reset-meter", quotaCalls: &calls}
	rota.Register(fakeMeter{fakeRefresher{p}})
	// Well inside QuotaTTL, so nothing but the reset can make the reading due.
	reset := now.Add(2 * time.Minute)
	p.quota = spentUntil(100, reset)

	s := openTemp(t)
	a := &rota.Account{ID: 1, Provider: "t-reset-meter", Order: 1, Token: rota.Token{Access: "a"}}
	s.Accounts = []*rota.Account{a}
	refresh := func(want int, why string) {
		t.Helper()
		if errs := s.Refresh(context.Background(), false); len(errs) != 0 {
			t.Fatal(errs)
		}
		if calls != want {
			t.Fatalf("%s: %d reads, want %d", why, calls, want)
		}
	}
	refresh(1, "the first look reads")

	now = reset.Add(-time.Second)
	refresh(1, "before the reset the cached reading is still the truth")
	now = reset.Add(ResetGrace / 2)
	refresh(1, "a moment after it the provider may not have rolled the window over yet")

	p.quota = spentUntil(3, reset.Add(5*time.Hour))
	now = reset.Add(ResetGrace + time.Second)
	refresh(2, "once the window has reset, the next look reads it again")
	if a.Percent() != 3 {
		t.Fatalf("the new reading is kept: %v", a.Percent())
	}

	// A provider still reporting a reset that has already passed is not asked
	// again on every look: the reading was taken after it, so it is fresh.
	p.quota = spentUntil(100, now.Add(-time.Second))
	if errs := s.Refresh(context.Background(), true); len(errs) != 0 {
		t.Fatal(errs)
	}
	now = now.Add(time.Minute)
	refresh(3, "a reading taken after the reset it reports is not out of date")
}

// NextReset is when a process keeping readings current should look again
// because a window resets: the soonest reset any reading predates, a grace
// after it. A reading nothing would read again is left out, and so is one
// already due, which the next look reads anyway.
func TestNextResetIsTheSoonestResetAReadingPredates(t *testing.T) {
	now := time.Now()
	clockAt(t, &now)
	rota.Register(fakeMeter{fakeRefresher{&fakeProvider{name: "t-reset-next"}}})
	rota.Register(&fakeProvider{name: "t-reset-unmetered"})
	at := now.Add(-time.Minute).UnixMilli()
	reading := func(id int, provider string, resets ...time.Duration) *rota.Account {
		q := &rota.Quota{}
		for _, r := range resets {
			q.Windows = append(q.Windows, rota.Window{Name: "w", Percent: 100, ResetsAt: rota.When{Time: now.Add(r)}})
		}
		return &rota.Account{ID: id, Provider: provider, Token: rota.Token{Access: "a"}, Quota: q, QuotaAt: at}
	}
	s := openTemp(t)
	if got := s.NextReset(); !got.IsZero() {
		t.Fatalf("an empty store waits on nothing: %v", got)
	}

	dead := reading(1, "t-reset-next", 10*time.Second)
	dead.Dead = true
	key := reading(2, "t-reset-next", 20*time.Second)
	key.Credential = "apikey"
	s.Accounts = []*rota.Account{
		dead, key,
		reading(3, "t-reset-unmetered", 30*time.Second),
		reading(4, "t-reset-next", -2*time.Minute),  // reset before the reading was taken
		reading(5, "t-reset-next", -30*time.Second), // reset since, so already due
		reading(6, "t-reset-next", 2*time.Hour, 40*time.Minute),
		reading(7, "t-reset-next", time.Hour),
		{ID: 8, Provider: "t-reset-next", Token: rota.Token{Access: "a"}}, // no reading at all
	}
	if got, want := s.NextReset(), now.Add(40*time.Minute+ResetGrace); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	now = now.Add(40*time.Minute + ResetGrace)
	if got, want := s.NextReset(), now.Add(20*time.Minute); !got.Equal(want) {
		t.Fatalf("a reset already due gives way to the next one: got %v, want %v", got, want)
	}
}

// A window that resets while the provider has asked rota to wait is read once
// the wait is over, and not a moment before: the reset makes the reading due,
// and the wait holds it back. NextReset, which a server sleeps on, says the
// later of the two — the reset's moment when the wait ends first, and the
// wait's end when the reset passes during it, since a look at the reset
// would read nothing.
func TestAResetDuringAWaitIsReadWhenTheWaitEnds(t *testing.T) {
	now := time.Now().Truncate(time.Second) // a wait is kept to the millisecond
	clockAt(t, &now)
	calls := 0
	p := &fakeProvider{name: "t-reset-wait", quotaCalls: &calls}
	rota.Register(fakeMeter{fakeRefresher{p}})
	reset := now.Add(2 * time.Minute)
	p.quota = spentUntil(100, reset)

	s := openTemp(t)
	a := &rota.Account{ID: 1, Provider: "t-reset-wait", Order: 1, Token: rota.Token{Access: "a"}}
	s.Accounts = []*rota.Account{a}
	if errs := s.Refresh(context.Background(), false); len(errs) != 0 || calls != 1 {
		t.Fatalf("the first look reads: %v, %d reads", errs, calls)
	}
	look := func(why string) {
		t.Helper()
		errs := s.Refresh(context.Background(), false)
		if calls != 1 {
			t.Fatalf("%s: %d reads", why, calls)
		}
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "is not asked again until") {
			t.Fatalf("%s: one line saying why: %v", why, errs)
		}
	}

	a.QuotaWait = rota.QuotaWait{Until: reset.Add(-time.Minute).UnixMilli(), Status: 429, Count: 1}
	if got, want := s.NextReset(), reset.Add(ResetGrace); !got.Equal(want) {
		t.Fatalf("a wait that ends before the reset leaves it be: got %v, want %v", got, want)
	}

	end := reset.Add(10 * time.Minute)
	a.QuotaWait = rota.QuotaWait{Until: end.UnixMilli(), Status: 429, Count: 1}
	if got := s.NextReset(); !got.Equal(end) {
		t.Fatalf("a reset that passes during the wait is looked at when the wait ends: got %v, want %v", got, end)
	}
	now = reset.Add(ResetGrace + time.Second)
	look("the window has reset, and the provider still asks rota to wait")
	if got := s.NextReset(); !got.Equal(end) {
		t.Fatalf("the reset passed, and the wait's end is still when to look: got %v, want %v", got, end)
	}
	now = end.Add(-time.Millisecond)
	look("a moment before the wait ends")

	p.quota = spentUntil(3, reset.Add(5*time.Hour))
	now = end
	if errs := s.Refresh(context.Background(), false); len(errs) != 0 || calls != 2 {
		t.Fatalf("the wait is over, and the reset still makes the reading due: %v, %d reads", errs, calls)
	}
	if a.Percent() != 3 || a.QuotaWait != (rota.QuotaWait{}) {
		t.Fatalf("the new reading is kept and ends the wait: %v %+v", a.Percent(), a.QuotaWait)
	}
	if got, want := s.NextReset(), reset.Add(5*time.Hour+ResetGrace); !got.Equal(want) {
		t.Fatalf("the next reset is the new reading's: got %v, want %v", got, want)
	}
}
