package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/professor93/rota/internal/events"
	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// The account events against a real store, as the sweeps leave it, and
// across a server's start, apart from events_accounts_test.go for their
// length; the other helpers are that file's.

// acctStarted is a server started on a store that holds blob, with the
// default refresh_every: its keeper takes the first look itself, as a
// daemon's does as it starts, and this waits for it. Its first sweep is two
// minutes off, so the test's own looks (acctSweep) are the sweeps after.
func acctStarted(t *testing.T, blob string) *Server {
	t.Helper()
	// Short, as the harness's: the store holds a socket's address.
	dir, err := os.MkdirTemp("", "rota")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(blob), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Token: "t", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		s.accounts.mu.Lock()
		looked := s.accounts.last != nil
		s.accounts.mu.Unlock()
		if looked {
			return s
		}
		if time.Now().After(end) {
			t.Fatal("the server took no first look at its accounts within 5s")
		}
	}
}

/* ---------------------------------------------------------------- sweeps --- */

// The account events follow what a sweep left in the store: the first
// reading the server takes is the baseline, and the next one past the
// threshold is said.
func TestAccountEventsFollowTheSweep(t *testing.T) {
	s := acctServer(t, `{"ordered":true,"nextId":2,"accounts":[
	 {"id":1,"provider":"claude","email":"one@x.com","order":1,"threshold":60,"token":{"accessToken":"tok-1"},
	  "quota":{"windows":[{"name":"5h","percent":50}]}}]}`)
	read := func(pct float64) func(*store.Store) {
		return func(st *store.Store) {
			a := st.Find(1)
			a.Quota.Windows[0].Percent = pct
			acctTake(a)
		}
	}
	acctSweep(t, s)
	acctEdit(t, s, read(50))
	acctSweep(t, s)
	acctEdit(t, s, read(70))
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

// A daemon that starts after a night off says nothing of what happened
// while no daemon ran (R6): three accounts read at 95% before it started,
// their windows reset since, read again at 2% by its first sweep, give no
// event at all. That reading is the baseline, and the next is said.
func TestNothingFromBeforeTheStartIsSaid(t *testing.T) {
	before := time.Now().Add(-3 * time.Hour).UnixMilli()
	reset := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	var accounts []string
	for id := 1; id <= 3; id++ {
		accounts = append(accounts, fmt.Sprintf(`{"id":%d,"provider":"claude","email":"n%d@x.com","order":%d,"threshold":80,`+
			`"token":{"accessToken":"tok-%d"},"quotaAt":%d,"quota":{"windows":[{"name":"5h","percent":95,"resetsAt":%q}]}}`,
			id, id, id, id, before, reset))
	}
	s := acctStarted(t, `{"ordered":true,"nextId":4,"accounts":[`+strings.Join(accounts, ",")+`]}`)

	readAt2 := func(ids ...int) func(*store.Store) {
		return func(st *store.Store) {
			for _, id := range ids {
				a := st.Find(id)
				a.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 2,
					ResetsAt: rota.When{Time: time.Now().Add(5 * time.Hour)}}}}
				acctTake(a)
			}
		}
	}
	// Its first sweep reads #1 and #2; #3's endpoint turns it away, and its
	// reading is still the one from before, until the sweep after.
	acctEdit(t, s, readAt2(1, 2))
	acctSweep(t, s)
	acctEdit(t, s, readAt2(3))
	acctSweep(t, s)
	if evs := acctAccountEvents(s); len(evs) != 0 {
		t.Fatalf("the first sweeps after a night off said %+v", evs)
	}

	acctEdit(t, s, func(st *store.Store) {
		a := st.Find(1)
		a.Quota.Windows[0].Percent = 85
		acctTake(a)
	})
	acctSweep(t, s)
	evs := acctAccountEvents(s)
	if len(evs) != 2 || evs[0].Type != events.AccountThreshold || evs[0].Account != 1 ||
		evs[1].Type != events.AccountRotated || evs[1].Account != 2 || evs[1].Reason != "spent" {
		t.Fatalf("#1 at 85%% after the baseline said %+v", evs)
	}
}

// A daemon restarted in seconds, for an upgrade, still says a crossing its
// first sweep finds: what the daemon before it read within two sweeps of
// this one's start counts as this one's own. 70% read a minute before the
// stop, the start 5s later, 85% at the first sweep: one account.threshold.
func TestACrossingOverAQuickRestartIsSaid(t *testing.T) {
	read := time.Now().Add(-time.Minute - 5*time.Second).UnixMilli()
	resets := time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339)
	s := acctStarted(t, fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
	 {"id":1,"provider":"claude","email":"one@x.com","order":1,"threshold":80,"token":{"accessToken":"tok-1"},
	  "quotaAt":%d,"quota":{"windows":[{"name":"5h","percent":70,"resetsAt":%q}]}},
	 {"id":2,"provider":"claude","email":"two@x.com","order":2,"threshold":80,"token":{"accessToken":"tok-2"},
	  "quotaAt":%d,"quota":{"windows":[{"name":"5h","percent":10,"resetsAt":%q}]}}]}`, read, resets, read, resets))

	at := func(pct float64) func(*store.Store) {
		return func(st *store.Store) {
			a := st.Find(1)
			a.Quota.Windows[0].Percent = pct
			acctTake(a)
		}
	}
	acctEdit(t, s, at(85))
	acctSweep(t, s) // the first sweep
	acctEdit(t, s, at(90))
	acctSweep(t, s)
	evs := published(s, events.AccountThreshold)
	if len(evs) != 1 || evs[0].Account != 1 || evs[0].Reason != "up" || acctPercent(evs[0]) != 85 {
		t.Fatalf("70%% before a 5s restart and 85%% after it said %+v", evs)
	}
}
