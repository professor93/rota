package rotation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

// An account spent until its window resets is picked again once it has: the
// reading that said spent is read again, although it is younger than the
// cache, and the rotation decides from what the provider says now. The
// clock is moved rather than waited on, and the provider is a usage endpoint
// on this machine.
func TestTheRotationPicksAnAccountAgainOnceItsWindowResets(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := rota.Now
	rota.Now = func() time.Time { return now }
	t.Cleanup(func() { rota.Now = old })
	// Well inside QuotaTTL, so only the reset can make the reading due.
	reset := now.Add(2 * time.Minute)

	var (
		mu    sync.Mutex
		asked = map[string]int{}
		spent = true
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		who := r.Header.Get("Authorization")
		asked[who]++
		switch {
		case who == "Bearer second":
			fmt.Fprintf(w, `{"five_hour":{"utilization":10,"resets_at":%q}}`, now.Add(4*time.Hour).Format(time.RFC3339))
		case spent:
			fmt.Fprintf(w, `{"five_hour":{"utilization":100,"resets_at":%q}}`, reset.Format(time.RFC3339))
		default:
			fmt.Fprintf(w, `{"five_hour":{"utilization":4,"resets_at":%q}}`, reset.Add(5*time.Hour).Format(time.RFC3339))
		}
	}))
	t.Cleanup(srv.Close)
	endpoint := rota.ClaudeEndpoints.Usage
	rota.ClaudeEndpoints.Usage = srv.URL
	t.Cleanup(func() { rota.ClaudeEndpoints.Usage = endpoint })

	s := openTemp(t)
	s.Accounts = []*rota.Account{
		{ID: 1, Provider: "claude", Order: 1, Token: rota.Token{Access: "first"}},
		{ID: 2, Provider: "claude", Order: 2, Token: rota.Token{Access: "second"}},
	}
	choose := func(want int, why string) {
		t.Helper()
		got, err := Choose(context.Background(), s, 0)
		if err != nil {
			t.Fatalf("%s: %v", why, err)
		}
		if got.ID != want {
			t.Fatalf("%s: picked %d, want %d", why, got.ID, want)
		}
	}
	reads := func(first, second int, why string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if asked["Bearer first"] != first || asked["Bearer second"] != second {
			t.Fatalf("%s: %v", why, asked)
		}
	}

	choose(2, "the first account is spent")
	reads(1, 1, "both are read once")
	now = reset.Add(-time.Second)
	choose(2, "until its window resets")
	reads(1, 1, "the cache holds until then")

	mu.Lock()
	spent = false
	mu.Unlock()
	now = reset.Add(store.ResetGrace + time.Second)
	choose(1, "once its window has reset, the first account is first again")
	reads(2, 1, "only the reading that predates a reset is read again")
}
