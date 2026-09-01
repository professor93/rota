package wire

import (
	"slices"
	"strings"
	"testing"

	rota "github.com/professor93/rota/lib"
)

func byName(fs []Field, name string) *Field {
	for i := range fs {
		if fs[i].Name == name {
			return &fs[i]
		}
	}
	return nil
}

// The description and the rule are in different packages now. This is what
// keeps them from drifting: a field lib refuses to some flavor that nobody
// describes is a field a form silently omits, and a caller then cannot tell
// why their request was rejected.
func TestEveryRestrictedFieldIsDescribed(t *testing.T) {
	all := Fields("")
	for _, name := range rota.RestrictedFields() {
		if byName(all, name) == nil {
			t.Errorf("lib restricts %q to some CLIs, but nothing here describes it", name)
		}
	}
}

// And the other way: a description carries the flavors lib gave it, never a
// list of its own.
func TestDescriptionsTakeTheirFlavorsFromTheLibrary(t *testing.T) {
	for _, f := range Fields("") {
		want := rota.FlavorsOf(f.Name)
		if !slices.Equal(f.Flavors, want) {
			t.Errorf("%s: flavors %v, want %v from lib", f.Name, f.Flavors, want)
		}
	}
}

func TestFieldsFollowTheProvider(t *testing.T) {
	all := Fields("")
	if len(all) < 40 {
		t.Fatalf("only %d fields", len(all))
	}

	cl := Fields("claude")
	m := byName(cl, "model")
	if m == nil || m.Kind != "enum" || !slices.Contains(m.Enum, "claude-opus-5") || m.Default != "claude-opus-5" {
		t.Fatalf("claude model field: %+v", m)
	}
	if e := byName(cl, "effort"); e == nil || e.Default != "high" || len(e.Enum) != 5 {
		t.Fatalf("claude effort field: %+v", e)
	}
	if byName(cl, "sandbox") != nil || byName(cl, "permission_mode") == nil {
		t.Fatal("claude must not be offered codex-only fields")
	}

	cx := Fields("codex")
	if byName(cx, "permission_mode") != nil || byName(cx, "sandbox") == nil || byName(cx, "prompt") == nil {
		t.Fatal("codex field set")
	}
	if d := byName(cx, "sandbox"); d == nil || len(d.Dangerous) != 1 {
		t.Fatalf("dangerous values must be marked: %+v", d)
	}
}

// What a provider accepts comes from its own catalog, so one description
// serves all four without offering anyone a value they would refuse.
func TestAcceptedValuesComeFromTheProvider(t *testing.T) {
	for _, c := range []struct{ provider, kind string }{
		{"claude", "enum"}, {"grok", "enum"}, {"kimi", "enum"},
	} {
		f := byName(Fields(c.provider), "permission_mode")
		if f == nil {
			t.Fatalf("%s has permission modes and must be offered the field", c.provider)
		}
		if !slices.Equal(f.Enum, rota.PermissionModes(c.provider)) {
			t.Errorf("%s: %v, want the catalog's %v", c.provider, f.Enum, rota.PermissionModes(c.provider))
		}
	}
	if byName(Fields("codex"), "permission_mode") != nil {
		t.Error("codex has no permission mode: it sandboxes instead")
	}

	// codex names its sandboxes; grok takes a profile from its own config,
	// which rota cannot enumerate, so the field is free text rather than a
	// list with nothing in it.
	if f := byName(Fields("codex"), "sandbox"); f == nil || f.Kind != "enum" ||
		!slices.Equal(f.Enum, rota.Sandboxes("codex")) {
		t.Errorf("codex sandbox: %+v", f)
	}
	if f := byName(Fields("grok"), "sandbox"); f == nil || f.Kind != "string" || len(f.Enum) != 0 {
		t.Errorf("grok sandbox: %+v", f)
	}
	if byName(Fields("kimi"), "sandbox") != nil {
		t.Error("kimi has no sandbox setting")
	}
}

// Before a provider is known there is nothing to enumerate: a union of four
// CLIs' values would offer combinations none of them takes.
func TestTheUnionOffersNoValues(t *testing.T) {
	for _, f := range Fields("") {
		if len(f.Enum) != 0 {
			t.Errorf("%s offers %v before a provider is chosen", f.Name, f.Enum)
		}
		if f.Kind == "enum" {
			t.Errorf("%s is an enum with nothing to choose from", f.Name)
		}
	}
}

// A field named twice is rendered twice by any form that walks the list.
func TestEachFieldIsNamedOnce(t *testing.T) {
	for _, provider := range []string{"", "claude", "codex", "grok", "kimi"} {
		seen := map[string]bool{}
		for _, f := range Fields(provider) {
			if seen[f.Name] {
				t.Errorf("%q: %q is described more than once", provider, f.Name)
			}
			seen[f.Name] = true
		}
	}
}

// The vendor spellings are gone from the request vocabulary, so a caller
// cannot learn one and find it works on only one provider.
func TestVendorSpellingsAreNotRequestFields(t *testing.T) {
	for _, gone := range []string{"output_schema", "fork", "no_session_persistence"} {
		if byName(Fields(""), gone) != nil {
			t.Errorf("%q is still a request field", gone)
		}
	}
}

func TestEveryFieldExplainsItself(t *testing.T) {
	for _, f := range Fields("") {
		if f.Label == "" || f.Help == "" {
			t.Fatalf("%s has no label or help: %+v", f.Name, f)
		}
		if len(f.Help) < 25 || !strings.HasSuffix(f.Help, ".") {
			t.Fatalf("%s: help must be a sentence, got %q", f.Name, f.Help)
		}
		if f.Group == "" {
			t.Fatalf("%s has no group", f.Name)
		}
	}
	// The fields most requests set must be marked, and must come first.
	for _, provider := range []string{"claude", "codex"} {
		primaries := 0
		for _, f := range Fields(provider) {
			if f.Primary {
				primaries++
			}
		}
		if primaries < 4 {
			t.Fatalf("%s: only %d primary fields", provider, primaries)
		}
		if first := Fields(provider)[0]; first.Name != "prompt" || !first.Primary {
			t.Fatalf("%s: the prompt must come first: %+v", provider, first)
		}
	}
	// A file upload is a request field like any other, so a form offers it.
	if f := byName(Fields("claude"), "files"); f == nil || f.Kind != "files" {
		t.Fatalf("files field: %+v", f)
	}
}

func BenchmarkFields(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = Fields("claude")
	}
}

// BenchmarkFieldsAll is what /v1/schema costs: every field of every
// provider, built fresh on each request.
func BenchmarkFieldsAll(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		for _, p := range rota.Providers() {
			_ = Fields(p)
		}
	}
}
