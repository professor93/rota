package api

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// These are about the account events: what one sweep's reading of the
// accounts says against the one before it. Most are tables over
// diffAccounts with accounts built in memory; the rest go through a server,
// for the sessions an account.threshold names and for a real store.

/* --------------------------------------------------------------- helpers --- */

// acctNow is when the in-memory looks are compared, unless a step moves it.
var acctNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

// acctOn is account id, named acct<id>@x.com, at pct of its 5h window, with
// order and threshold.
func acctOn(id, order, threshold int, pct float64) *rota.Account {
	return &rota.Account{ID: id, Provider: "claude", Email: fmt.Sprintf("acct%d@x.com", id),
		Order: order, Threshold: threshold,
		Quota: &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: pct}}}}
}

// acctWindow sets a's window name to pct, resetting at resets, adding it
// when a has none of that name.
func acctWindow(a *rota.Account, name string, pct float64, resets time.Time) {
	for i := range a.Quota.Windows {
		if a.Quota.Windows[i].Name == name {
			a.Quota.Windows[i].Percent, a.Quota.Windows[i].ResetsAt = pct, rota.When{Time: resets}
			return
		}
	}
	a.Quota.Windows = append(a.Quota.Windows, rota.Window{Name: name, Percent: pct, ResetsAt: rota.When{Time: resets}})
}

// acctDrop takes a's window name away.
func acctDrop(a *rota.Account, name string) {
	var keep []rota.Window
	for _, w := range a.Quota.Windows {
		if w.Name != name {
			keep = append(keep, w)
		}
	}
	a.Quota.Windows = keep
}

// acctWatcher keeps the last look the way a server does, for a table that
// steps through one test's looks.
type acctWatcher struct {
	last accountsLook
	now  time.Time
}

// look diffs accounts against the last look, keeps what diffAccounts kept,
// and gives what it said.
func (w *acctWatcher) look(accounts ...*rota.Account) []events.Event {
	evs, kept := diffAccounts(w.last, lookAccounts(accounts), w.now)
	w.last = kept
	return evs
}

// acctOf is the events of type typ among evs.
func acctOf(evs []events.Event, typ string) []events.Event {
	var out []events.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// acctOne is the one event of type typ among evs, failing the test unless
// there is exactly one.
func acctOne(t *testing.T, evs []events.Event, typ, step string) events.Event {
	t.Helper()
	got := acctOf(evs, typ)
	if len(got) != 1 {
		t.Fatalf("%s: %d %s, want one; every event: %+v", step, len(got), typ, evs)
	}
	return got[0]
}

// acctNone fails the test when evs has an event of one of types, or any
// event at all when no type is given.
func acctNone(t *testing.T, evs []events.Event, step string, types ...string) {
	t.Helper()
	for _, e := range evs {
		if len(types) == 0 || slices.Contains(types, e.Type) {
			t.Fatalf("%s: said %+v, want nothing", step, e)
		}
	}
}

// acctPercent is an event's percent, or -1 when it carries none.
func acctPercent(e events.Event) float64 {
	if e.Percent == nil {
		return -1
	}
	return *e.Percent
}

// acctServer is a foreground server with no sweeps of its own, on a store
// that holds blob: the test sweeps it (acctSweep).
func acctServer(t *testing.T, blob string) *Server {
	t.Helper()
	h := eventsServer(t)
	if err := os.WriteFile(filepath.Join(h.dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	return h.api
}

// acctSweep is what a sweep ends with: the store open, its accounts looked
// at, the store closed.
func acctSweep(t *testing.T, s *Server) {
	t.Helper()
	st, err := s.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s.lookAtAccounts(st)
}

// acctEdit changes the store under s with f and saves it.
func acctEdit(t *testing.T, s *Server, f func(*store.Store)) {
	t.Helper()
	st, err := s.openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f(st)
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

// acctSession lists a session on s for the rest of the test: id, on
// account, called name, started at started, and ended or still running.
// It runs nothing; it is what a sweep finds listed.
func acctSession(t *testing.T, s *Server, id string, account int, name string, started time.Time, ended bool) {
	t.Helper()
	ts := &termSession{s: s, id: id, account: account, name: name, started: started, ended: ended}
	s.termsMu.Lock()
	s.terms[id] = ts
	s.termsMu.Unlock()
	t.Cleanup(func() { s.dropTerminal(id) })
}

// acctAccountEvents is every account.* event s has published.
func acctAccountEvents(s *Server) []events.Event {
	return published(s, events.AccountThreshold, events.AccountReset, events.AccountDead, events.AccountRotated)
}

/* -------------------------------------------------------------- baseline --- */

// What is true at the first look is a baseline, said by nobody: an account
// over its threshold, a dead one, and the rotation's first choice. A
// restarted daemon must not announce them all again (Review Focus 4), and a
// store looked at twice unchanged says nothing either.
func TestTheFirstLookIsABaseline(t *testing.T) {
	spent := acctOn(1, 1, 80, 85)
	dead := acctOn(2, 2, 80, 10)
	dead.Dead, dead.DeadReason = true, "x"
	first := acctOn(3, 3, 80, 10)
	look := lookAccounts([]*rota.Account{spent, dead, first})
	evs, kept := diffAccounts(accountsLook{}, look, acctNow)
	acctNone(t, evs, "the first look")
	if kept.first != 3 || len(kept.byID) != 3 || !kept.byID[1].spent || !kept.byID[2].dead {
		t.Fatalf("the first look kept %+v, want all three with #3 first", kept)
	}

	s := acctServer(t, `{"ordered":true,"nextId":4,"accounts":[
	 {"id":1,"provider":"claude","email":"one@x.com","order":1,"threshold":80,"token":{"accessToken":"tok-1"},
	  "quota":{"windows":[{"name":"5h","percent":85}]}},
	 {"id":2,"provider":"claude","email":"two@x.com","order":2,"dead":true,"deadReason":"x","token":{"accessToken":"tok-2"}},
	 {"id":3,"provider":"claude","email":"three@x.com","order":3,"token":{"accessToken":"tok-3"},
	  "quota":{"windows":[{"name":"5h","percent":10}]}}]}`)
	acctSweep(t, s)
	acctSweep(t, s)
	if evs := acctAccountEvents(s); len(evs) != 0 {
		t.Fatalf("two looks at an unchanged store said %+v", evs)
	}
}

// An account first seen in a later look is a baseline of its own: nothing
// about it is said, its being first in the rotation included, until it
// changes.
func TestANewAccountIsABaseline(t *testing.T) {
	w := acctWatcher{now: acctNow}
	one := acctOn(1, 1, 80, 85) // spent: nothing is first
	acctNone(t, w.look(one), "the baseline")

	two := acctOn(2, 0, 80, 90)
	two.Dead, two.DeadReason = true, "x"
	three := acctOn(3, 2, 80, 10)
	acctNone(t, w.look(one, two, three), "two accounts first seen, one of them first in the rotation")
	if w.last.first != 3 {
		t.Fatalf("the rotation's first is #%d, want #3", w.last.first)
	}
	acctNone(t, w.look(one, two, three), "the same again")

	two.Quota.Windows[0].Percent = 10
	e := acctOne(t, w.look(one, two, three), events.AccountThreshold, "the new account under its limit")
	if e.Account != 2 || e.Reason != "down" {
		t.Fatalf("the new account's first change said %+v", e)
	}
}

/* ------------------------------------------------------------- threshold --- */

// An account crossing its threshold is said once each way, with the window
// its reading's top comes from; a reading that goes on rising says nothing
// more. The account's live sessions are named; an ended one, and one on
// another account, are not.
func TestThresholdCrossesUpAndBackDown(t *testing.T) {
	h := eventsServer(t)
	s := h.api
	t0 := time.Now().Add(-time.Hour)
	acctSession(t, s, "t-refactor", 1, "refactor", t0, false)
	acctSession(t, s, "t-old", 1, "old", t0.Add(-time.Hour), true)
	acctSession(t, s, "t-other", 2, "other", t0, false)

	a := acctOn(1, 1, 80, 70)
	acctWindow(a, "7d", 5, time.Time{})
	a.Quota.Windows = append(a.Quota.Windows, rota.Window{Name: "7d opus", Percent: 99, Scoped: true})
	other := acctOn(2, 2, 80, 10)
	look := func() []events.Event {
		before := len(published(s, events.AccountThreshold))
		s.watchAccounts(lookAccounts([]*rota.Account{a, other}))
		return published(s, events.AccountThreshold)[before:]
	}
	if evs := look(); len(evs) != 0 {
		t.Fatalf("the baseline said %+v", evs)
	}

	a.Quota.Windows[0].Percent = 82
	up := acctOne(t, look(), events.AccountThreshold, "70% to 82%")
	if up.Reason != "up" || acctPercent(up) != 82 || up.Limit != 80 || up.Window != "5h" ||
		up.Account != 1 || up.Name != "acct1@x.com" || up.Session != "" ||
		up.Text != "reached its 80% limit: 5h at 82%; refactor runs on it" {
		t.Fatalf("70%% to 82%% said %+v", up)
	}
	if got := events.Sentence(up); got != "acct1@x.com reached its 80% limit: 5h at 82%; refactor runs on it" {
		t.Fatalf("it reads %q", got)
	}

	a.Quota.Windows[0].Percent = 90
	if evs := look(); len(evs) != 0 {
		t.Fatalf("82%% to 90%% said %+v", evs)
	}

	a.Quota.Windows[0].Percent = 12
	down := acctOne(t, look(), events.AccountThreshold, "90% to 12%")
	if down.Reason != "down" || acctPercent(down) != 12 || down.Limit != 80 || down.Window != "5h" ||
		down.Text != "is under its 80% limit again: 5h at 12%; refactor runs on it" {
		t.Fatalf("90%% to 12%% said %+v", down)
	}

	// The window named is the one the account's percent comes from: here
	// the week's, the five hours being low and the scoped one not counted.
	acctWindow(a, "7d", 84.6, time.Time{})
	week := acctOne(t, look(), events.AccountThreshold, "the week at 85%")
	if week.Reason != "up" || week.Window != "7d" || week.Text != "reached its 80% limit: 7d at 85%; refactor runs on it" {
		t.Fatalf("the week at 85%% said %+v", week)
	}
}

// At most three sessions are named, oldest first, and the rest counted.
func TestThresholdNamesAtMostThreeSessions(t *testing.T) {
	h := eventsServer(t)
	s := h.api
	if got := s.accountRunsOn(1); got != "" {
		t.Fatalf("with no session it reads %q", got)
	}
	t0 := time.Now().Add(-time.Hour)
	want := []string{
		"; s1 runs on it",
		"; s1 and s2 run on it",
		"; s1, s2 and s3 run on it",
		"; s1, s2, s3 and 1 more run on it",
		"; s1, s2, s3 and 2 more run on it",
	}
	for i, w := range want {
		n := strconv.Itoa(i + 1)
		acctSession(t, s, "t"+n, 1, "s"+n, t0.Add(time.Duration(i)*time.Minute), false)
		if got := s.accountRunsOn(1); got != w {
			t.Fatalf("with %d sessions it reads %q, want %q", i+1, got, w)
		}
	}
}

// A reading that fails is no reading: it changes no latch, so the one after
// it is compared with the last that came (Review Focus 4).
func TestAFailedReadingChangesNothing(t *testing.T) {
	w := acctWatcher{now: acctNow}
	a := acctOn(1, 1, 80, 82)
	b := acctOn(2, 2, 80, 10)
	check := func(step string, evs []events.Event) {
		t.Helper()
		acctNone(t, evs, step)
		if !w.last.byID[1].spent || w.last.first != 2 {
			t.Fatalf("%s: #1 is kept as %+v and #%d is first, want #1 spent and #2 first", step, w.last.byID[1], w.last.first)
		}
	}
	check("82%", w.look(a, b))
	a.Quota = nil
	check("no reading", w.look(a, b))
	a.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 85}}}
	check("85%", w.look(a, b))
	a.Quota = nil
	check("no reading again", w.look(a, b))

	a.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 12}}}
	evs := w.look(a, b)
	if e := acctOne(t, evs, events.AccountThreshold, "12% after no reading"); e.Reason != "down" {
		t.Fatalf("12%% after no reading said %+v", e)
	}
	if e := acctOne(t, evs, events.AccountRotated, "12% after no reading"); e.Account != 1 || e.Reason != "back" {
		t.Fatalf("12%% after no reading said %+v", e)
	}
}

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

/* ------------------------------------------------------------------ dead --- */

// An account that needs a login is said once each time it comes to need
// one; a new login re-arms it silently. A login that ended is said too,
// though nothing has marked the account dead.
func TestADeadAccountIsSaidOnce(t *testing.T) {
	w := acctWatcher{now: acctNow}
	a := acctOn(1, 1, 0, 10)
	b := acctOn(2, 2, 0, 10)
	b.Extra = map[string]string{"refresh_token_expires_at": strconv.FormatInt(time.Now().Add(24*time.Hour).UnixMilli(), 10)}
	acctNone(t, w.look(a, b), "the baseline")

	a.Dead, a.DeadReason = true, "x"
	e := acctOne(t, w.look(a, b), events.AccountDead, "dead")
	if e.Reason != "login" || e.Text != "needs a login: x" || e.Account != 1 || e.Name != "acct1@x.com" || e.Session != "" {
		t.Fatalf("dead said %+v", e)
	}
	acctNone(t, w.look(a, b), "still dead", events.AccountDead)
	a.Dead, a.DeadReason = false, ""
	acctNone(t, w.look(a, b), "alive again", events.AccountDead)
	a.Dead, a.DeadReason = true, "y"
	if e := acctOne(t, w.look(a, b), events.AccountDead, "dead again"); e.Text != "needs a login: y" {
		t.Fatalf("dead again said %+v", e)
	}

	ended := time.Now().Add(-time.Hour)
	b.Extra["refresh_token_expires_at"] = strconv.FormatInt(ended.UnixMilli(), 10)
	e = acctOne(t, w.look(a, b), events.AccountDead, "its login ended")
	want := "needs a login: its login ended on " + ended.UTC().Format(time.DateOnly) + " UTC; rota login 2 makes a new one"
	if e.Reason != "login" || e.Account != 2 || e.Text != want {
		t.Fatalf("its login ended said %+v, want the text %q", e, want)
	}
	acctNone(t, w.look(a, b), "its login still ended", events.AccountDead)
}

// A dead reason is the server's or rota's own words, and may repeat a
// credential somebody typed; the event it is said in does not.
func TestADeadReasonRepeatsNoKey(t *testing.T) {
	h := eventsServer(t)
	s := h.api
	key := "sk-ant-oat01-AAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	a := acctOn(1, 1, 0, 10)
	s.watchAccounts(lookAccounts([]*rota.Account{a}))
	a.Dead, a.DeadReason = true, "the server refused "+key
	s.watchAccounts(lookAccounts([]*rota.Account{a}))
	evs := published(s, events.AccountDead)
	if len(evs) != 1 || strings.Contains(evs[0].Text, key) || evs[0].Text != "needs a login: the server refused sk-ant-[not repeated]" {
		t.Fatalf("a dead reason holding a key said %+v", evs)
	}
}

/* -------------------------------------------------------------- rotation --- */

// The rotation's first choice moving is said with why: the account before
// it is spent, needs a login, is out of credit, left the rotation or was
// removed; or the new one can be used again, or was put ahead; or there is
// none left.
func TestTheRotationMoves(t *testing.T) {
	w := acctWatcher{now: acctNow}
	one, two := acctOn(1, 1, 80, 10), acctOn(2, 2, 80, 10)
	accounts := []*rota.Account{one, two}
	step := func(name string, account int, reason, text string) {
		t.Helper()
		e := acctOne(t, w.look(accounts...), events.AccountRotated, name)
		wantName := ""
		if account != 0 {
			wantName = fmt.Sprintf("acct%d@x.com", account)
		}
		if e.Account != account || e.Reason != reason || e.Name != wantName || e.Text != text {
			t.Fatalf("%s said %+v, want #%d %s %q", name, e, account, reason, text)
		}
	}
	quiet := func(name string) {
		t.Helper()
		acctNone(t, w.look(accounts...), name, events.AccountRotated)
	}
	quiet("the baseline, #1 first")

	one.Quota.Windows[0].Percent = 85
	step("#1 spent", 2, "spent", "is first in the rotation now: acct1@x.com reached its 80% limit")
	two.Dead, two.DeadReason = true, "x"
	step("#2 dead and #1 still spent", 0, "none",
		"No account is available in the rotation: every account in it is spent, needs a login or is out of credit")
	one.Quota.Windows[0].Percent = 5
	step("#1 reset", 1, "back", "is first in the rotation now: it can be used again")
	two.Dead, two.DeadReason = false, ""
	quiet("#2 alive again behind #1")
	one.Order = 0
	step("#1 given order 0", 2, "left", "is first in the rotation now: acct1@x.com left the rotation")
	one.Order = 1
	step("#1 given order 1 again", 1, "order", "is first in the rotation now: the rotation was reordered")
	accounts = []*rota.Account{two}
	step("#1 removed", 2, "removed", "is first in the rotation now: acct1@x.com was removed")

	three := acctOn(3, 3, 80, 10)
	accounts = []*rota.Account{two, three}
	quiet("#3 first seen behind #2")
	two.Dead, two.DeadReason = true, "x"
	step("#2 dead", 3, "dead", "is first in the rotation now: acct2@x.com needs a login")
	two.Dead, two.DeadReason = false, ""
	step("#2 alive", 2, "back", "is first in the rotation now: it can be used again")
	two.NoCreditUntil = time.Now().Add(time.Hour).UnixMilli()
	step("#2 out of credit", 3, "credit", "is first in the rotation now: acct2@x.com is out of credit")
	two.NoCreditUntil = 0
	step("#2 with credit", 2, "back", "is first in the rotation now: it can be used again")

	two.Order, three.Order = 0, 0
	step("nothing in the rotation", 0, "none", "No account is available in the rotation: no account is in it")
	quiet("still nothing")
}

/* ---------------------------------------------------------------- sweeps --- */

// The account events follow what a sweep left in the store.
func TestAccountEventsFollowTheSweep(t *testing.T) {
	s := acctServer(t, `{"ordered":true,"nextId":2,"accounts":[
	 {"id":1,"provider":"claude","email":"one@x.com","order":1,"threshold":60,"token":{"accessToken":"tok-1"},
	  "quota":{"windows":[{"name":"5h","percent":50}]}}]}`)
	acctSweep(t, s)
	acctEdit(t, s, func(st *store.Store) { st.Find(1).Quota.Windows[0].Percent = 70 })
	acctSweep(t, s)
	evs := published(s, events.AccountThreshold)
	if len(evs) != 1 {
		t.Fatalf("70%% past a threshold of 60 said %+v", evs)
	}
	if e := evs[0]; e.Reason != "up" || e.Account != 1 || e.Name != "one@x.com" || acctPercent(e) != 70 || e.Limit != 60 ||
		e.Text != "reached its 60% limit: 5h at 70%" || e.ID == 0 || e.Time.IsZero() {
		t.Fatalf("70%% past a threshold of 60 said %+v", e)
	}
}
