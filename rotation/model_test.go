package rotation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// The first word of the window's name decides, once a leading "Claude"
	// is set aside: "Claude Fable" is Fable's window and nobody else's, and
	// "Opus 4.1" is Opus's.
	for _, c := range []struct {
		label, model string
		passed       bool
	}{
		{"Claude Fable", "fable", true},
		{"Claude Fable", "claude-fable-5", true},
		{"Claude Fable", "claude-opus-5", false},
		{"Claude Fable", "claude-haiku-4-5-20251001", false},
		{"Opus 4.1", "opus", true},
		{"Opus 4.1", "claude-opus-5", true},
		{"Opus 4.1", "fable", false},
		{"Opus 4.1", "claude-fable-5", false},
		{"Claude", "claude-opus-5", false}, // a label of no model matches none
		{"Claude", "claude-fable-5", false},
	} {
		a := withFable(1, 1, 10, 0)
		a.Quota.Windows[1].Name, a.Quota.Windows[1].Percent = c.label, 100
		if got := spentOn(a, c.model) != ""; got != c.passed {
			t.Errorf("a spent %q window for a %q request: passed by %v, want %v", c.label, c.model, got, c.passed)
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

// A request that names a model passes by an account whose own catalog — the
// one its run is checked against — does not list that model, as an id or an
// alias, for a later account whose catalog does. A provider that takes ids
// beyond its list (grok) lists only its list. When no account lists the
// model, the choice is the one it always was, and the run's own check
// answers; when the only accounts that list it have spent its window, that
// window is what the refusal names.
func TestARequestForAModelGoesToAnAccountThatListsIt(t *testing.T) {
	s := openTemp(t)
	codex := func(id, order int, models ...string) *rota.Account {
		a := &rota.Account{ID: id, Provider: "codex", Order: order}
		if len(models) > 0 {
			var rows []string
			for _, m := range models {
				rows = append(rows, `{"slug":"`+m+`","display_name":"`+m+`","visibility":"list"}`)
			}
			home := s.Home(a)
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "models_cache.json"),
				[]byte(`{"models":[`+strings.Join(rows, ",")+`]}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return a
	}
	claude := func(id, order int, fable float64) *rota.Account {
		return &rota.Account{ID: id, Provider: "claude", Order: order, Quota: &rota.Quota{Windows: []rota.Window{
			{Name: "5h", Percent: 10, Primary: true}, {Name: "Fable", Percent: fable, Scoped: true}}}}
	}
	grok := func(id, order int) *rota.Account { return &rota.Account{ID: id, Provider: "grok", Order: order} }
	choose := func(model string) (*rota.Account, error) {
		return Choose(askmodel.With(context.Background(), model), s, 0)
	}
	for _, c := range []struct {
		what     string
		accounts []*rota.Account
		model    string
		want     int
	}{
		{"claude lists fable, codex does not", []*rota.Account{codex(1, 1), claude(2, 2, 0)}, "fable", 2},
		{"claude lists its own id", []*rota.Account{codex(1, 1), claude(2, 2, 0)}, "claude-fable-5", 2},
		{"no model, the queue's order", []*rota.Account{codex(1, 1), claude(2, 2, 0)}, "", 1},
		{"nobody lists it: the first, as before", []*rota.Account{codex(1, 1)}, "fable", 1},
		{"a Claude id nobody lists stays on claude, not grok", []*rota.Account{claude(1, 1, 0), grok(2, 2)}, "claude-opus-5-5", 1},
		{"codex's own catalog lists it: not passed by for grok", []*rota.Account{codex(11, 1, "gpt-5.7"), grok(2, 2)}, "gpt-5.7", 11},
		{"one codex lacks it, the next lists it", []*rota.Account{codex(21, 1, "gpt-5.6-sol"), codex(22, 2, "gpt-5.5")}, "gpt-5.5", 22},
	} {
		s.Accounts = c.accounts
		if got, err := choose(c.model); err != nil || got.ID != c.want {
			t.Errorf("%s: %v %v, want account %d", c.what, got, err, c.want)
		}
	}

	s.Accounts = []*rota.Account{claude(1, 1, 100), codex(2, 2)}
	if _, err := choose("fable"); !errors.Is(err, ErrNone) || !strings.Contains(err.Error(), "Fable") {
		t.Fatalf("the spent window is named, not the catalog that cannot help: %v", err)
	}
}
