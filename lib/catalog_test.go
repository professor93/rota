package rota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeCatalog struct {
	*fakeProvider
	models  []Model
	efforts []string
	dm, de  string
}

func (f fakeCatalog) Models() []Model            { return f.models }
func (f fakeCatalog) Efforts() []string          { return f.efforts }
func (f fakeCatalog) Defaults() (string, string) { return f.dm, f.de }

func TestCatalogAnswersPerProvider(t *testing.T) {
	Register(fakeCatalog{fakeProvider: &fakeProvider{name: "t-cat"},
		models:  []Model{{ID: "big-1", Aliases: []string{"big"}}, {ID: "small-1"}},
		efforts: []string{"low", "high"}, dm: "big-1", de: "high"})
	Register(&fakeProvider{name: "t-nocat"})

	if got := Models("t-cat"); len(got) != 2 || got[0].ID != "big-1" {
		t.Fatalf("%+v", got)
	}
	if got := Efforts("t-cat"); len(got) != 2 || got[1] != "high" {
		t.Fatalf("%v", got)
	}
	m, e := Defaults("t-cat")
	if m != "big-1" || e != "high" {
		t.Fatalf("%q %q", m, e)
	}
	if Models("t-nocat") != nil || Efforts("t-nocat") != nil {
		t.Fatal("a provider without a catalog reports none")
	}
	if m, e := Defaults("t-nocat"); m != "" || e != "" {
		t.Fatalf("%q %q", m, e)
	}
	if Models("nope") != nil {
		t.Fatal("unknown provider")
	}
}

func TestModelIsCheckedAgainstItsOwnProvider(t *testing.T) {
	Register(fakeCatalog{fakeProvider: &fakeProvider{name: "t-a"},
		models: []Model{{ID: "a-1", Aliases: []string{"a"}}}, efforts: []string{"low"}, dm: "a-1", de: "low"})
	Register(fakeCatalog{fakeProvider: &fakeProvider{name: "t-b"},
		models: []Model{{ID: "b-1"}}, dm: "b-1"})

	if got, err := ResolveModel("t-a", "a"); err != nil || got != "a-1" {
		t.Fatalf("an alias resolves to its id: %q %v", got, err)
	}
	if got, err := ResolveModel("t-a", "A-1"); err != nil || got != "a-1" {
		t.Fatalf("case does not matter: %q %v", got, err)
	}
	err := mustErr(t, func() error { _, e := ResolveModel("t-a", "b-1"); return e })
	if !strings.Contains(err.Error(), "t-a") || !strings.Contains(err.Error(), "b-1") || !strings.Contains(err.Error(), "a-1") {
		t.Fatalf("the error must name the provider, the bad model and the good ones: %v", err)
	}
	if got, err := ResolveModel("t-nocat", "anything"); err != nil || got != "anything" {
		t.Fatalf("a provider without a catalog accepts what it is given: %q %v", got, err)
	}
	if got, err := ResolveModel("t-a", ""); err != nil || got != "a-1" {
		t.Fatalf("no model means the provider default: %q %v", got, err)
	}
}

func TestEffortIsCheckedAndDisabledWhereUnsupported(t *testing.T) {
	if got, err := ResolveEffort("t-a", "low"); err != nil || got != "low" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := ResolveEffort("t-a", ""); err != nil || got != "low" {
		t.Fatalf("no effort means the provider default: %q %v", got, err)
	}
	err := mustErr(t, func() error { _, e := ResolveEffort("t-a", "extreme"); return e })
	if !strings.Contains(err.Error(), "low") {
		t.Fatalf("%v", err)
	}
	// t-b publishes no effort levels: asking for one is an error, not a
	// silently dropped flag.
	err = mustErr(t, func() error { _, e := ResolveEffort("t-b", "low"); return e })
	if !strings.Contains(err.Error(), "t-b") || !strings.Contains(err.Error(), "effort") {
		t.Fatalf("%v", err)
	}
	if got, err := ResolveEffort("t-b", ""); err != nil || got != "" {
		t.Fatalf("without levels, no effort is sent at all: %q %v", got, err)
	}
}

func TestSpecUsesTheProviderCatalog(t *testing.T) {
	argv, err := specArgv(Spec{Prompt: "p"}, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	dm, de := Defaults("claude")
	if dm == "" || de == "" {
		t.Fatal("claude must ship a default model and effort")
	}
	if !strings.Contains(got, "--model "+dm) || !strings.Contains(got, "--effort "+de) {
		t.Fatalf("defaults must be sent explicitly: %q", got)
	}
	if _, err := specArgv(Spec{Prompt: "p", Model: "gpt-5"}, "claude", nil); err == nil {
		t.Fatal("a codex model must be refused for a claude account")
	}
	if _, err := specArgv(Spec{Prompt: "p", Model: "definitely-not-a-model"}, "codex", nil); err == nil {
		t.Fatal("an invented model must be refused")
	}
	argv, err = specArgv(Spec{Prompt: "p", Model: "opus"}, "claude", nil)
	if err != nil || !strings.Contains(strings.Join(argv, " "), "--model claude-opus-5") {
		t.Fatalf("an alias must reach the CLI as a full id: %v %v", argv, err)
	}
}

func TestEveryShippedProviderHasACatalog(t *testing.T) {
	// kimi is left out on purpose: its -m takes an alias defined in the
	// account's own config file, so rota has no list to check against and
	// says so by shipping none.
	for _, name := range []string{"claude", "codex", "grok"} {
		models := Models(name)
		if len(models) == 0 {
			t.Fatalf("%s ships no models", name)
		}
		dm, de := Defaults(name)
		if _, err := ResolveModel(name, dm); err != nil {
			t.Fatalf("%s default model %q is not in its own catalog: %v", name, dm, err)
		}
		if _, err := ResolveEffort(name, de); err != nil {
			t.Fatalf("%s default effort %q is not in its own catalog: %v", name, de, err)
		}
		if de != "" && len(Efforts(name)) == 0 {
			t.Fatalf("%s names a default effort but no levels", name)
		}
		seen := map[string]bool{}
		for _, m := range models {
			for _, k := range append([]string{m.ID}, m.Aliases...) {
				if seen[strings.ToLower(k)] {
					t.Fatalf("%s lists %q twice", name, k)
				}
				seen[strings.ToLower(k)] = true
			}
		}
	}
}

func mustErr(t *testing.T, fn func() error) error {
	t.Helper()
	err := fn()
	if err == nil {
		t.Fatal("expected an error")
	}
	return err
}

type fakeAccountCatalog struct{ fakeCatalog }

func (f fakeAccountCatalog) ModelsFor(a *Account, home string) []Model {
	if home == "" {
		return nil
	}
	return []Model{{ID: "from-" + home}}
}

func TestModelsCanDependOnTheAccount(t *testing.T) {
	Register(fakeAccountCatalog{fakeCatalog{fakeProvider: &fakeProvider{name: "t-acct-cat"},
		models: []Model{{ID: "static-1"}}, dm: "static-1"}})
	a := &Account{ID: 1, Provider: "t-acct-cat"}
	if got := ModelsFor(a, "h1"); len(got) != 1 || got[0].ID != "from-h1" {
		t.Fatalf("%+v", got)
	}
	// No account-specific answer: the provider's own list still applies.
	if got := ModelsFor(a, ""); len(got) != 1 || got[0].ID != "static-1" {
		t.Fatalf("%+v", got)
	}
	// A provider with only a static catalog is unaffected.
	if got := ModelsFor(&Account{Provider: "claude"}, "h"); len(got) == 0 || got[0].ID != "claude-opus-5" {
		t.Fatalf("%+v", got)
	}
}

func TestCodexReadsTheModelsItsOwnAccountMayUse(t *testing.T) {
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(`{"models":[
	  {"slug":"gpt-5.6-terra","display_name":"Terra","visibility":"list","priority":2},
	  {"slug":"gpt-5.6-luna","display_name":"Luna","visibility":"list","priority":3},
	  {"slug":"codex-auto-review","visibility":"hide","priority":40},
	  {"slug":"gpt-5.5","display_name":"5.5","visibility":"list","priority":7}]}`), 0o600)
	a := &Account{ID: 3, Provider: "codex"}
	got := ModelsFor(a, home)
	if len(got) != 3 || got[0].ID != "gpt-5.6-terra" || got[2].ID != "gpt-5.5" {
		t.Fatalf("account models, in the CLI's own order, hidden ones dropped: %+v", got)
	}
	// A missing or unreadable cache falls back to what codex ships.
	if got := ModelsFor(a, t.TempDir()); len(got) == 0 || got[0].ID != "gpt-5.6-sol" {
		t.Fatalf("fallback: %+v", got)
	}
	// A model this account cannot use is refused before anything is spent.
	spec := Spec{Prompt: "p", Model: "gpt-5.6-sol"}
	if err := spec.CheckFor(a, home, nil); err == nil || !strings.Contains(err.Error(), "gpt-5.6-terra") {
		t.Fatalf("must be refused and list what the account has: %v", err)
	}
	if err := (Spec{Prompt: "p", Model: "gpt-5.6-terra"}).CheckFor(a, home, nil); err != nil {
		t.Fatalf("an entitled model must pass: %v", err)
	}
}

func TestCodexSendsNoModelUnlessAsked(t *testing.T) {
	argv, err := specArgv(Spec{Prompt: "p"}, "codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if strings.Contains(got, "-m ") {
		t.Fatalf("without a named model the CLI must choose one the account has: %q", got)
	}
	if !strings.Contains(got, "-c model_reasoning_effort=medium") {
		t.Fatalf("the default effort still applies: %q", got)
	}
}
