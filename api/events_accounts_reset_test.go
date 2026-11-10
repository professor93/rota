package api

import (
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	rota "github.com/professor93/rota/lib"
)

// The account events about a window coming back (account.reset), apart from
// events_accounts_test.go for their length; the helpers are that file's.

/* ----------------------------------------------------------------- reset --- */

// A window coming back is said once: when its reset moves on, when its
// reset has passed and its reading fell, or when its reset has passed and
// it is gone. Never for a window that had not been used, nor for a reset
// that moved by seconds, nor for a scoped window.
func TestAWindowResetIsSaidOnce(t *testing.T) {
	w := acctWatcher{now: acctNow}
	r := acctNow.Add(time.Hour)
	a := acctOn(1, 1, 0, 95)
	acctWindow(a, "5h", 95, r)
	a.Quota.Windows = append(a.Quota.Windows, rota.Window{Name: "7d opus", Percent: 90, Scoped: true,
		ResetsAt: rota.When{Time: r}})
	acctNone(t, w.look(a), "the baseline")

	acctWindow(a, "5h", 3, r.Add(5*time.Hour))
	e := acctOne(t, w.look(a), events.AccountReset, "the reset moved on by 5h")
	if e.Window != "5h" || acctPercent(e) != 95 || e.Limit != 100 || e.Account != 1 || e.Name != "acct1@x.com" ||
		e.Text != "has its 5h window back: it had reached 95%" {
		t.Fatalf("the reset moved on by 5h said %+v", e)
	}
	acctNone(t, w.look(a), "the same reading again", events.AccountReset)
	acctWindow(a, "5h", 3, r.Add(5*time.Hour+30*time.Second))
	acctNone(t, w.look(a), "the reset 30s later", events.AccountReset)
	acctWindow(a, "5h", 0, r.Add(5*time.Hour+30*time.Second))
	acctNone(t, w.look(a), "the window at 0%", events.AccountReset)
	acctWindow(a, "5h", 0, r.Add(10*time.Hour))
	acctNone(t, w.look(a), "a window at 0% that resets", events.AccountReset)

	// Its reset passed and its reading fell, the reset not yet moved on.
	r2 := acctNow.Add(2 * time.Hour)
	acctWindow(a, "5h", 40, r2)
	acctNone(t, w.look(a), "40%", events.AccountReset)
	w.now = r2.Add(time.Minute)
	acctWindow(a, "5h", 10, r2)
	if e := acctOne(t, w.look(a), events.AccountReset, "fell after its reset"); e.Window != "5h" || acctPercent(e) != 40 {
		t.Fatalf("fell after its reset said %+v", e)
	}
	acctNone(t, w.look(a), "the same after its reset", events.AccountReset)

	// Its reset passed and it is gone.
	r3 := w.now.Add(time.Hour)
	acctWindow(a, "7d", 50, r3)
	acctNone(t, w.look(a), "a week first seen", events.AccountReset)
	w.now = r3.Add(time.Minute)
	acctDrop(a, "7d")
	if e := acctOne(t, w.look(a), events.AccountReset, "gone after its reset"); e.Window != "7d" ||
		e.Text != "has its 7d window back: it had reached 50%" {
		t.Fatalf("gone after its reset said %+v", e)
	}

	// The scoped window moved on long ago, and nothing was said of it.
	acctWindow(a, "7d opus", 1, w.now.Add(7*24*time.Hour))
	acctNone(t, w.look(a), "a scoped window back", events.AccountReset)
}

// A provider is slow to roll a window over, and its clock wanders: one
// reset is said once though its reading and its reset time come back in
// different looks, and a reset time that jitters, or moves earlier, while
// the window fills says nothing. A window that comes back early, its
// reading falling as its reset moves on, is said.
func TestAResetIsSaidOnceWhateverTheProviderLags(t *testing.T) {
	w := acctWatcher{now: acctNow}
	r := acctNow.Add(time.Minute) // passes between the first look and the second
	now := r.Add(time.Minute)     // the second look's, and every one after until the last
	slow := acctOn(1, 0, 0, 95)   // the reading rolls over first, the reset two looks later
	acctWindow(slow, "5h", 95, r)
	quick := acctOn(2, 0, 0, 95) // the reset catches up at the next look
	acctWindow(quick, "5h", 95, r)
	r3 := acctNow.Add(3 * time.Hour)
	jitter := acctOn(3, 0, 0, 50)
	acctWindow(jitter, "5h", 50, r3.Add(-40*time.Second))
	drift := acctOn(4, 0, 0, 40) // due 30s after the second look, then said to be 2m before it
	acctWindow(drift, "5h", 40, now.Add(30*time.Second))
	all := []*rota.Account{slow, quick, jitter, drift}
	acctNone(t, w.look(all...), "the baseline")

	w.now = now
	acctWindow(slow, "5h", 10, r)
	acctWindow(quick, "5h", 10, r)
	acctWindow(jitter, "5h", 52, r3.Add(40*time.Second))
	acctWindow(drift, "5h", 41, now.Add(-2*time.Minute))
	evs := acctOf(w.look(all...), events.AccountReset)
	if len(evs) != 2 || evs[0].Account != 1 || evs[1].Account != 2 || acctPercent(evs[0]) != 95 || acctPercent(evs[1]) != 95 {
		t.Fatalf("two rolled over, the third's reset jittered by 80s and the fourth's moved earlier: %+v", evs)
	}

	acctWindow(slow, "5h", 12, r)
	acctWindow(quick, "5h", 12, r.Add(5*time.Hour))
	acctWindow(jitter, "5h", 55, r3.Add(-5*time.Minute))
	acctNone(t, w.look(all...), "#1 still on its old reset, #2's caught up, #3's moved earlier", events.AccountReset)
	acctWindow(slow, "5h", 12, r.Add(5*time.Hour))
	acctNone(t, w.look(all...), "#1's reset caught up", events.AccountReset)
	acctWindow(slow, "5h", 30, r.Add(5*time.Hour))
	acctWindow(quick, "5h", 30, r.Add(5*time.Hour))
	acctNone(t, w.look(all...), "both filling again", events.AccountReset)

	// #3 comes back hours before its reset: its reading falls as its reset
	// moves on.
	acctWindow(jitter, "5h", 0, r3.Add(5*time.Hour))
	if e := acctOne(t, w.look(all...), events.AccountReset, "#3 back early"); e.Account != 3 || acctPercent(e) != 55 {
		t.Fatalf("#3 back early said %+v", e)
	}

	// The next reset is a reset of its own.
	w.now = r.Add(5*time.Hour + time.Minute)
	acctWindow(slow, "5h", 2, r.Add(10*time.Hour))
	if e := acctOne(t, w.look(all...), events.AccountReset, "#1's next reset"); e.Account != 1 || acctPercent(e) != 30 {
		t.Fatalf("#1's next reset said %+v", e)
	}
}

// A window whose reset had already passed when the server first compared
// it reset while nobody compared: when the provider catches up, nothing is
// said of the reset. Its reading falling is said, since both readings are
// the server's. So for an account never read before, and for one first seen.
func TestAResetThatPassedBeforeTheFirstReadingIsNotSaid(t *testing.T) {
	w := acctWatcher{now: acctNow}
	r := acctNow.Add(-time.Hour) // passed, the provider not yet rolled over
	unread := acctOn(1, 1, 80, 0)
	unread.Quota = nil
	acctNone(t, w.look(unread), "the baseline, #1 never read")

	unread.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 95, ResetsAt: rota.When{Time: r}}}}
	newcomer := acctOn(2, 2, 80, 95)
	acctWindow(newcomer, "5h", 95, r)
	all := []*rota.Account{unread, newcomer}
	acctNone(t, w.look(all...), "#1's first reading and #2 first seen, both at 95% on a reset that passed")
	acctNone(t, w.look(all...), "the same, the provider still behind")

	acctWindow(unread, "5h", 2, acctNow.Add(4*time.Hour))
	acctWindow(newcomer, "5h", 2, acctNow.Add(4*time.Hour))
	evs := w.look(all...)
	acctNone(t, evs, "the provider caught up", events.AccountReset)
	if downs := acctOf(evs, events.AccountThreshold); len(downs) != 2 || downs[0].Reason != "down" || downs[1].Reason != "down" {
		t.Fatalf("the provider caught up and the thresholds said %+v", downs)
	}
	if e := acctOne(t, evs, events.AccountRotated, "the provider caught up"); e.Account != 1 || e.Reason != "back" {
		t.Fatalf("the provider caught up and the rotation said %+v", e)
	}
}
