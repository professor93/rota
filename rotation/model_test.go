package rotation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/professor93/rota/internal/askmodel"
	rota "github.com/professor93/rota/lib"
)

// withFable is an account whose whole-account window is at percent and
// whose weekly window for one model, Fable, is at fable.
func withFable(id, order int, percent, fable float64) *rota.Account {
	return &rota.Account{ID: id, Provider: "t-rot-model", Order: order, Quota: &rota.Quota{Windows: []rota.Window{
		{Name: "5h", Percent: percent, Primary: true},
		{Name: "Fable", Percent: fable, Scoped: true},
	}}}
}

func init() { rota.Register(provider{name: "t-rot-model"}) }

// A request that names a model counts that model's window as it counts the
// account's own: at the account's threshold the rotation moves on, for that
// request alone. The window's name is matched as a word of the model's,
// without regard to case; a window of another model, and a request that names
// none, change nothing.
func TestAModelsWindowCountsForARequestThatNamesIt(t *testing.T) {
	spent := withFable(1, 1, 10, 80)
	spent.Threshold = 80
	list := []*rota.Account{spent, withFable(2, 2, 50, 79)}
	s := openTemp(t)
	s.Accounts = list
	for _, c := range []struct {
		model string
		want  int
	}{
		{"", 1},
		{"opus", 1},
		{"claude-opus-5", 1},
		{"fable", 2},
		{"Fable", 2},
		{"claude-fable-5", 2},
		{"claude-fable-5[1m]", 2},
		{"fabled", 1}, // a word, not a prefix
	} {
		got, err := Choose(askmodel.With(context.Background(), c.model), s, 0)
		if err != nil || got.ID != c.want {
			t.Errorf("model %q: got %v, %v; want account %d", c.model, got, err, c.want)
		}
	}
	// What the rotation offers with no model is unchanged too.
	if got, err := Pick(list); err != nil || got.ID != 1 || !Available(spent) || Spent(spent) {
		t.Fatalf("Pick, Available and Spent know no model: %v %v", got, err)
	}
	// By id it runs, whatever was asked.
	if got, err := Choose(askmodel.With(context.Background(), "fable"), s, 1); err != nil || got.ID != 1 {
		t.Fatalf("by id: %v %v", got, err)
	}
}

// When the window of the model asked for leaves nothing, the refusal names
// the window, so the person knows another model, or a raised threshold,
// would run.
func TestARefusalNamesTheModelsWindow(t *testing.T) {
	s := openTemp(t)
	s.Accounts = []*rota.Account{withFable(1, 1, 10, 100), withFable(2, 2, 20, 100)}
	_, err := Choose(askmodel.With(context.Background(), "fable"), s, 0)
	if !errors.Is(err, ErrNone) || !strings.Contains(err.Error(), "Fable") {
		t.Fatalf("got %v", err)
	}
	_, err = Choose(context.Background(), s, 0)
	if err != nil {
		t.Fatalf("with no model named, the account's own windows decide: %v", err)
	}
	s.Accounts = []*rota.Account{withFable(1, 1, 100, 0)}
	_, err = Choose(askmodel.With(context.Background(), "fable"), s, 0)
	if !errors.Is(err, ErrNone) || strings.Contains(err.Error(), "Fable") {
		t.Fatalf("an account spent on its own windows is not blamed on the model's: %v", err)
	}
}
