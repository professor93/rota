package rota

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A Registry is a value an application composes, not only a process global:
// two of them hold different provider sets without seeing each other, and
// each carries its own default. The package-level functions stay, as the
// convenient face of DefaultRegistry.
func TestARegistryIsComposable(t *testing.T) {
	r := NewRegistry()
	r.Register(&fakeProvider{name: "only-mine"})
	r.Default = "only-mine"

	if _, err := r.Lookup(""); err != nil {
		t.Fatalf("the registry's own default resolves: %v", err)
	}
	if _, err := r.Lookup("claude"); err == nil {
		t.Fatal("a composed registry does not inherit the builtins")
	}
	if _, err := Lookup("claude"); err != nil {
		t.Fatal("the default registry still has them")
	}
	if names := r.Providers(); len(names) != 1 || names[0] != "only-mine" {
		t.Fatalf("its list is its own: %v", names)
	}
}

// A provider may name the CLI vocabulary it speaks, so a third-party
// provider driving Claude Code gets claude's argv builder instead of the
// closed name switch answering "unknown".
type flavoredFake struct{ fakeProvider }

func (flavoredFake) Flavor() string { return "claude" }

func TestAProviderMayNameItsFlavor(t *testing.T) {
	Register(&flavoredFake{fakeProvider{name: "t-my-claude"}})
	if got := Flavor("t-my-claude"); got != "claude" {
		t.Fatalf("the provider said claude, Flavor says %q", got)
	}
	if got := Flavor("claude"); got != "claude" {
		t.Fatalf("builtins unchanged: %q", got)
	}
}

// grok's model list is documented as the floor, not the ceiling — the CLI
// shows more once an account is signed in — so an id beyond the list passes
// through for the CLI to judge, while claude's complete catalog still
// refuses a typo before it costs a session.
func TestAFloorCatalogPassesUnknownModelsThrough(t *testing.T) {
	got, err := ResolveModel("grok", "grok-9-preview")
	if err != nil || got != "grok-9-preview" {
		t.Fatalf("a floor catalog lets the CLI judge: %q %v", got, err)
	}
	if got, err := ResolveModel("grok", "grok-4.5"); err != nil || got != "grok-4.5" {
		t.Fatalf("known ids still resolve: %q %v", got, err)
	}
	if _, err := ResolveModel("claude", "claude-nope"); err == nil {
		t.Fatal("a complete catalog still refuses a typo")
	}
}

// kimi's login is delegated: nothing is pasted, and saying "apikey" made
// every consumer prompt for a key nobody has. The kind says what the flow
// is, and a pasted secret is refused rather than silently dropped.
func TestKimiSaysItsLoginIsDelegatedAndRefusesAPaste(t *testing.T) {
	l, err := Begin(context.Background(), "kimi")
	if err != nil {
		t.Fatal(err)
	}
	if l.Kind != "delegated" || !l.Delegated {
		t.Fatalf("kind %q delegated=%v", l.Kind, l.Delegated)
	}
	if _, err := l.Complete(context.Background(), "sk-pasted"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("a pasted secret must be refused, not dropped: %v", err)
	}
	tok, err := l.Complete(context.Background(), "")
	if err != nil || !tok.Delegated {
		t.Fatalf("an empty finish registers the delegated account: %+v %v", tok, err)
	}
	if !strings.HasPrefix(tok.Identity.UUID, "kimi-") {
		t.Fatalf("identity: %+v", tok.Identity)
	}
}
