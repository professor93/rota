package rota

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fieldFlavors is a hand-written list of which CLI understands what, and the
// argv builders are the code that actually knows. A list that disagrees with
// them is worse than no list: the whole point of it is to refuse a field
// that would otherwise be accepted and ignored, and a stale entry does
// exactly the thing it was written to prevent.
//
// These tests tie the list to the behaviour rather than to another list.

// setByTag sets the Spec field with this JSON name to a plausible non-zero
// value, and reports whether it managed to.
func setByTag(s *Spec, name string) bool {
	v := reflect.ValueOf(s).Elem()
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag != name {
			continue
		}
		f := v.Field(i)
		switch f.Interface().(type) {
		case string:
			f.SetString("x")
		case bool:
			f.SetBool(true)
		case int:
			f.SetInt(1)
		case float64:
			f.SetFloat(1)
		case []string:
			f.Set(reflect.ValueOf([]string{"x"}))
		case map[string]string:
			f.Set(reflect.ValueOf(map[string]string{"k": "v"}))
		case json.RawMessage:
			f.Set(reflect.ValueOf(json.RawMessage(`{}`)))
		case []json.RawMessage:
			f.Set(reflect.ValueOf([]json.RawMessage{json.RawMessage(`{}`)}))
		default:
			return false
		}
		return true
	}
	return false
}

// Every field the list restricts must exist on Spec. A name nobody can set
// is a rule that never fires.
func TestEveryRestrictedFieldIsASpecField(t *testing.T) {
	for _, name := range RestrictedFields() {
		if !setByTag(&Spec{}, name) {
			t.Errorf("%q is restricted but is not a settable Spec field", name)
		}
	}
}

// The refusal actually happens, for every field and every CLI the list says
// does not understand it — and it names the field, so a caller can act on it.
func TestARestrictedFieldIsRefusedByName(t *testing.T) {
	flavors := []string{"claude", "codex", "grok", "kimi"}
	for _, name := range RestrictedFields() {
		understood := FlavorsOf(name)
		for _, flavor := range flavors {
			if slices.Contains(understood, flavor) {
				continue
			}
			spec := Spec{Prompt: "p"}
			if !setByTag(&spec, name) {
				continue // reported by the test above
			}
			err := spec.Check(flavor, nil)
			if err == nil {
				t.Errorf("%s accepted %q, which the list says it does not understand", flavor, name)
				continue
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("%s refused %q without naming it: %v", flavor, name, err)
			}
		}
	}
}

// And the other direction, where it matters most: a CLI the list says
// understands a permission-shaped field must actually do something with it.
//
// This is the failure the whole table exists to prevent. A field that is
// accepted and then ignored leaves a run less confined than the caller
// asked for, and reports success.
func TestAPermissionFieldTheListAllowsReachesTheCommandLine(t *testing.T) {
	for _, c := range []struct {
		field string
		set   func(*Spec)
	}{
		{"sandbox", func(s *Spec) { s.Sandbox = "read-only" }},
		{"permission_mode", func(s *Spec) { s.PermissionMode = "plan" }},
		{"restricted", func(s *Spec) { s.Restricted = true }},
		{"allowed_tools", func(s *Spec) { s.AllowedTools = []string{"Bash"} }},
		{"disallowed_tools", func(s *Spec) { s.DisallowedTools = []string{"Bash"} }},
		{"safe_mode", func(s *Spec) { s.SafeMode = true }},
		{"disable_web_search", func(s *Spec) { s.DisableWebSearch = true }},
		{"no_subagents", func(s *Spec) { s.NoSubagents = true }},
	} {
		for _, flavor := range FlavorsOf(c.field) {
			bare, err := specArgv(Spec{Prompt: "p"}, flavor, nil)
			if err != nil {
				t.Fatalf("%s: %v", flavor, err)
			}
			spec := Spec{Prompt: "p"}
			c.set(&spec)
			with, err := specArgv(spec, flavor, nil)
			if err != nil {
				t.Errorf("%s says it understands %q but refused it: %v", flavor, c.field, err)
				continue
			}
			if slices.Equal(bare, with) {
				t.Errorf("%s accepts %q and builds the same command line without it: %v",
					flavor, c.field, with)
			}
		}
	}
}
