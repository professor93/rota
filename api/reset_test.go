package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// resetter is a provider whose usage endpoint says the window has reset:
// account "a" is at 7% now, and every read is noted with when it came.
type resetter struct{}

var resetReads struct {
	sync.Mutex
	at []time.Time
}

func (resetter) Name() string                                               { return "t-api-reset" }
func (resetter) Begin(_ context.Context) (string, map[string]string, error) { return "", nil, nil }
func (resetter) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: "a"}, nil
}
func (resetter) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}
func (resetter) Quota(_ context.Context, access string) (*rota.Quota, error) {
	resetReads.Lock()
	defer resetReads.Unlock()
	resetReads.at = append(resetReads.at, time.Now())
	if access != "a" {
		return nil, fmt.Errorf("only account a is due, and %q was read", access)
	}
	return &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: 7, Primary: true,
		ResetsAt: rota.When{Time: time.Now().Add(5 * time.Hour)}}}}, nil
}

func init() { rota.Register(resetter{}) }

// A server reads an account again a moment after its window resets, not up
// to a whole sweep later: the sweep is an hour apart here, and the reading
// arrives all the same, and the rotation's choice moves back to the account
// that was spent. It is the sweep that was coming anyway, woken early, and
// nothing reads again before the reset or after it. The server runs in a
// bubble of its own whose clock moves only when everything in it waits, so
// the test says exactly when, on no machine's schedule.
func TestTheServerReadsAnAccountAgainOnceItsWindowResets(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resetReads.Lock()
		resetReads.at = nil
		resetReads.Unlock()
		start := time.Now()
		due := start.Add(90 * time.Second)
		// Inside the cache's lifetime and before the reset, so the reading
		// is fresh until the reset makes it due — and it does at `due`.
		at := start.Add(-time.Minute).UnixMilli()
		reset := due.Add(-store.ResetGrace).Format(time.RFC3339Nano)
		blob := fmt.Sprintf(`{"ordered":true,"nextId":3,"accounts":[
			{"id":1,"provider":"t-api-reset","order":1,"token":{"accessToken":"a"},"quotaAt":%d,
			 "quota":{"windows":[{"name":"5h","percent":100,"primary":true,"resetsAt":%q}]}},
			{"id":2,"provider":"t-api-reset","order":2,"token":{"accessToken":"b"},"quotaAt":%d,
			 "quota":{"windows":[{"name":"5h","percent":50,"primary":true}]}}]}`, at, reset, at)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
		srv, err := New(Options{Token: "t", Dir: dir, RefreshEvery: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Stop()
		handler := srv.Handler()
		choice := func() int {
			t.Helper()
			req := httptest.NewRequest("GET", "/v1/accounts", nil)
			req.Header.Set("Authorization", "Bearer t")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			var doc struct {
				Default int `json:"default"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("%d %s: %v", rec.Code, rec.Body, err)
			}
			return doc.Default
		}
		reads := func() []time.Time {
			resetReads.Lock()
			defer resetReads.Unlock()
			return append([]time.Time(nil), resetReads.at...)
		}
		// Until the moment d from the start, with the server done with
		// whatever that moment asked of it.
		until := func(d time.Duration) {
			time.Sleep(time.Until(start.Add(d)))
			synctest.Wait()
		}

		until(0)
		if got := choice(); got != 2 {
			t.Fatalf("before the reset the spent account is passed by: %d", got)
		}
		until(due.Sub(start) - time.Millisecond)
		if got := reads(); len(got) != 0 {
			t.Fatalf("read before the reset made the reading due: %v", got)
		}
		until(due.Sub(start))
		if got := reads(); len(got) != 1 || !got[0].Equal(due) {
			t.Fatalf("the window reset, and the account is read again then, once: %v, due %v", got, due)
		}
		if got := choice(); got != 1 {
			t.Fatalf("once read again, the account that was spent is the rotation's choice: %d", got)
		}
		until(time.Hour - time.Millisecond)
		if got := reads(); len(got) != 1 {
			t.Fatalf("one reset is one read, and nothing else is due before the sweep: %v", got)
		}
	})
}

// untilNext is how long the keeper sleeps: until the sweep, or until a reset
// if that is sooner — but never less than a second for a reset, so a clock
// that disagrees about when it is cannot make the keeper sweep without
// pause. A sweep that is due is due at once, as a ticker's would be.
func TestTheKeeperSleepsUntilTheSweepOrAReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now()
		for _, c := range []struct {
			why          string
			sweep, reset time.Duration // from now; reset 0 is none
			want         time.Duration
		}{
			{"no reset waits for the sweep", 2 * time.Minute, 0, 2 * time.Minute},
			{"a sooner reset comes first", 2 * time.Minute, 10 * time.Second, 10 * time.Second},
			{"a later reset waits for the sweep", 2 * time.Minute, time.Hour, 2 * time.Minute},
			{"a reset already past is a second off", 2 * time.Minute, -time.Hour, soonestReset},
			{"so is one sooner than a second", 2 * time.Minute, time.Millisecond, soonestReset},
			{"a sweep sooner than that still comes first", 200 * time.Millisecond, -time.Hour, 200 * time.Millisecond},
			{"a sweep already due is due now", -time.Second, 0, -time.Second},
		} {
			var reset time.Time
			if c.reset != 0 {
				reset = now.Add(c.reset)
			}
			if got := untilNext(now.Add(c.sweep), reset); got != c.want {
				t.Errorf("%s: %v, want %v", c.why, got, c.want)
			}
		}
	})
}
