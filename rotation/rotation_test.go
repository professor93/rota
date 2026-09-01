package rotation

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
	"github.com/professor93/rota/store"
)

func acct(id, order int, percent float64) *rota.Account {
	a := &rota.Account{ID: id, Provider: "claude", Order: order}
	if percent >= 0 {
		a.Quota = &rota.Quota{Windows: []rota.Window{{Name: "5h", Percent: percent}}}
	}
	return a
}

func ids(list []*rota.Account) []int {
	out := make([]int, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

func eq(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestQueueExcludesOrderZeroAndSortsByOrder(t *testing.T) {
	list := []*rota.Account{acct(1, 0, 0), acct(2, 3, 0), acct(3, 1, 0), acct(4, 2, 0)}
	eq(t, ids(Queue(list)), []int{3, 4, 2})
}

func TestQueueBreaksTiesByID(t *testing.T) {
	list := []*rota.Account{acct(7, 1, 0), acct(2, 1, 0), acct(5, 1, 0)}
	eq(t, ids(Queue(list)), []int{2, 5, 7})
}

func TestSortKeepsUnorderedAccountsLast(t *testing.T) {
	list := []*rota.Account{acct(1, 0, 0), acct(2, 2, 0), acct(3, 0, 0), acct(4, 1, 0)}
	Sort(list)
	eq(t, ids(list), []int{4, 2, 1, 3})
}

func TestPickTakesTheFirstAccountUnderItsThreshold(t *testing.T) {
	list := []*rota.Account{acct(1, 1, 100), acct(2, 2, 10), acct(3, 3, 0)}
	got, err := Pick(list)
	if err != nil || got.ID != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPickHonoursAPerAccountThreshold(t *testing.T) {
	first := acct(1, 1, 85)
	first.Threshold = 80
	list := []*rota.Account{first, acct(2, 2, 90)}
	got, err := Pick(list)
	if err != nil || got.ID != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPickSkipsDeadAccountsAndOnesLeftOutOfTheQueue(t *testing.T) {
	dead := acct(1, 1, 0)
	dead.Dead = true
	out := acct(2, 0, 0) // order 0: never picked, however healthy
	list := []*rota.Account{dead, out, acct(3, 5, 0)}
	got, err := Pick(list)
	if err != nil || got.ID != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPickReportsWhenTheRotationIsEmptyOrExhausted(t *testing.T) {
	if _, err := Pick(nil); !errors.Is(err, ErrNone) {
		t.Fatalf("empty store: %v", err)
	}
	if _, err := Pick([]*rota.Account{acct(1, 0, 0)}); !errors.Is(err, ErrNone) {
		t.Fatalf("nothing in the rotation: %v", err)
	}
	if _, err := Pick([]*rota.Account{acct(1, 1, 100), acct(2, 2, 100)}); !errors.Is(err, ErrNone) {
		t.Fatalf("everything spent: %v", err)
	}
}

func TestCutoffDefaultsToOneHundred(t *testing.T) {
	if got := Cutoff(&rota.Account{}); got != DefaultThreshold {
		t.Fatalf("got %d", got)
	}
	if got := Cutoff(&rota.Account{Threshold: 70}); got != 70 {
		t.Fatalf("got %d", got)
	}
}

/* ---------------------------------------------------- over a real store -- */

func openTemp(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBackfillOrdersAStoreThatPredatesRotation(t *testing.T) {
	s := openTemp(t)
	s.Accounts = []*rota.Account{
		{ID: 3, Provider: "claude"}, {ID: 1, Provider: "claude"}, {ID: 7, Provider: "codex"},
	}
	Backfill(s)

	want := map[int]int{1: 1, 3: 2, 7: 3}
	for _, a := range s.Accounts {
		if a.Order != want[a.ID] {
			t.Fatalf("account %d got order %d, want %d (ids in ascending order)", a.ID, a.Order, want[a.ID])
		}
	}
	if !s.Ordered {
		t.Fatal("the store must record that it has been ordered, so this happens exactly once")
	}
	// And it must reach the backend, or the next process backfills again
	// over whatever the person has since chosen.
	raw, err := s.Backend().Load()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"ordered": true`) {
		t.Fatalf("the backfill was not persisted: %s", raw)
	}
}

func TestBackfillKeepsADeliberatelyEmptyRotation(t *testing.T) {
	s := openTemp(t)
	s.Ordered = true
	s.Accounts = []*rota.Account{{ID: 1, Provider: "claude"}, {ID: 2, Provider: "claude"}}
	Backfill(s)
	for _, a := range s.Accounts {
		if a.Order != 0 {
			t.Fatalf("an already-ordered store must not be renumbered: %d is at %d", a.ID, a.Order)
		}
	}
}

func TestBackfillLeavesExistingOrdersAlone(t *testing.T) {
	s := openTemp(t)
	s.Accounts = []*rota.Account{
		{ID: 1, Provider: "claude", Order: 9}, {ID: 2, Provider: "claude"},
	}
	Backfill(s)
	if s.Accounts[0].Order != 9 || s.Accounts[1].Order != 10 {
		t.Fatalf("accounts already in the rotation keep their place, the rest join the end: %d, %d",
			s.Accounts[0].Order, s.Accounts[1].Order)
	}
}

func TestBackfillWritesNothingForAnEmptyStore(t *testing.T) {
	s := openTemp(t)
	Backfill(s)
	if s.Ordered {
		t.Fatal("an empty store has nothing to decide, and must not be written out to record that")
	}
}

func TestChooseFindsTheAccountAnIDNames(t *testing.T) {
	s := openTemp(t)
	s.Accounts = []*rota.Account{{ID: 4, Provider: "claude", Order: 1}}
	got, err := Choose(context.Background(), s, 4)
	if err != nil || got.ID != 4 {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := Choose(context.Background(), s, 9); !errors.Is(err, rota.ErrNoAccount) {
		t.Fatalf("got %v", err)
	}
}

func TestChooseRefreshesUsageBeforeChoosing(t *testing.T) {
	rota.Register(meter{provider{name: "t-rot-spent"},
		&rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 100, Primary: true}}}})
	rota.Register(meter{provider{name: "t-rot-free"},
		&rota.Quota{Windows: []rota.Window{{Name: "w", Percent: 3, Primary: true}}}})

	s := openTemp(t)
	s.Accounts = []*rota.Account{
		{ID: 1, Provider: "t-rot-spent", Order: 1},
		{ID: 2, Provider: "t-rot-free", Order: 2},
	}
	got, err := Choose(context.Background(), s, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 2 {
		t.Fatalf("picked account %d; the first is at 100%% and only a fresh reading says so", got.ID)
	}
	if s.Accounts[0].Percent() != 100 {
		t.Fatal("the readings taken on the way must be kept, not thrown away")
	}
}

func TestChooseReportsAnExhaustedRotation(t *testing.T) {
	s := openTemp(t)
	s.Accounts = []*rota.Account{{ID: 1, Provider: "claude", Order: 0}}
	if _, err := Choose(context.Background(), s, 0); !errors.Is(err, ErrNone) {
		t.Fatalf("got %v", err)
	}
}

// provider is a vendor with no network and no CLI.
type provider struct{ name string }

func (p provider) Name() string { return p.name }
func (p provider) Begin(_ context.Context) (string, map[string]string, error) {
	return "https://x/auth", map[string]string{"verifier": "v"}, nil
}
func (p provider) Complete(_ context.Context, code string, _ map[string]string) (*rota.Token, error) {
	return &rota.Token{Access: code}, nil
}
func (p provider) Launch(a *rota.Account, _ string) (*rota.Command, error) {
	return &rota.Command{Bin: "true"}, nil
}

type meter struct {
	provider
	quota *rota.Quota
}

func (m meter) Quota(context.Context, string) (*rota.Quota, error) { return m.quota, nil }

// rotOwns stands in for codex and grok, whose CLI owns the credential file.
type rotOwns struct{ rota.Provider }

func (rotOwns) Name() string                             { return "t-rot-owns" }
func (rotOwns) Adopt(a *rota.Account, home string) error { return nil }
func (rotOwns) Launch(*rota.Account, string) (*rota.Command, error) {
	return &rota.Command{Bin: "t-cli"}, nil
}
func (rotOwns) Begin(_ context.Context) (string, map[string]string, error) { return "", nil, nil }
func (rotOwns) Complete(context.Context, string, map[string]string) (*rota.Token, error) {
	return &rota.Token{}, nil
}

func init() { rota.Register(rotOwns{}) }

// The store carries the raw number; what an unset one means is decided here,
// so a caller asking for the threshold in force gets a real answer.
func TestCutoffReportsTheThresholdInForce(t *testing.T) {
	if Cutoff(&rota.Account{ID: 1, Provider: "claude", Threshold: 80}) != 80 {
		t.Fatal("a set threshold is its own answer")
	}
	if Cutoff(&rota.Account{ID: 2, Provider: "claude"}) != DefaultThreshold {
		t.Fatal("an unset threshold is reported as the one in force")
	}
}
