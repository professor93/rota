package rota

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// Scratch files go where the caller says. lib used to pick the system temp
// directory on its own — a storage decision — and a confined server could
// not even see where request data was landing.
func TestScratchFilesGoWhereTheCallerSays(t *testing.T) {
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r // the check resolves symlinks, so compare against the resolved dir
	}
	argv, err := specArgv(Spec{Prompt: "p", ScratchDir: dir}, "grok", nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range argv {
		if strings.HasPrefix(a, dir+string(filepath.Separator)) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the prompt file must land in the caller's directory: %v", argv)
	}
}

// Which settings sources a Claude Code run reads is the caller's choice.
// nil means the CLI's own default; an explicit empty list means none. The
// flag used to be emitted always — silently disabling every settings source
// for every caller, with no way to say "leave the CLI alone".
func TestSettingSourcesFollowTheCaller(t *testing.T) {
	argv, err := specArgv(Spec{Prompt: "p"}, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if a == "--setting-sources" {
			t.Fatalf("an unset field must not disable the CLI's own sources: %v", argv)
		}
	}
	argv, err = specArgv(Spec{Prompt: "p", SettingSources: []string{}}, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(argv, "--setting-sources", "") {
		t.Fatalf("an explicit empty list means none: %v", argv)
	}
	argv, err = specArgv(Spec{Prompt: "p", SettingSources: []string{"user"}}, "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasPair(argv, "--setting-sources", "user") {
		t.Fatalf("a named list passes through: %v", argv)
	}
}

func hasPair(argv []string, flag, val string) bool {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) && argv[i+1] == val {
			return true
		}
	}
	return false
}

// The output caps are the caller's, with today's values as defaults, and a
// run that hit one says so instead of silently shortening the answer.
func TestOutputCapsAreTheCallersAndTruncationIsSaid(t *testing.T) {
	bin := fakeCLI(t, "claude",
		`{"type":"system","subtype":"one"}
{"type":"system","subtype":"two"}
{"type":"system","subtype":"three"}`, "")
	Register(&fakeProvider{name: "t-caps", launched: &Command{Bin: bin, Env: []string{"F=1"}}})
	a := &Account{ID: 1, Provider: "t-caps"}
	a.Token.Access = "tok"

	var out bytes.Buffer
	lim := &Limits{AllowRawFlags: true, MaxEvents: 2}
	res, err := Run(context.Background(), a, "", nil,
		Spec{Prompt: "p", Stream: true, IncludeEvents: true, flavorOverride: "claude"}, lim, &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) > 2 {
		t.Fatalf("the caller said two events at most: %d", len(res.Events))
	}
	if !res.Truncated {
		t.Fatal("a hit cap must be said, not silent")
	}
}

// Which settings keys a mediated caller may not send is policy, and policy
// is data the operator supplies; the built-in list is only the default.
func TestTheSettingsDenylistIsTheOperatorsData(t *testing.T) {
	custom := &Limits{SettingsDenyKeys: []string{"model"}}
	err := Spec{Prompt: "p", Settings: json.RawMessage(`{"model":"x"}`)}.Check("claude", custom)
	if err == nil {
		t.Fatal("the operator's own denylist must bite")
	}
	// With a custom list, only that list applies...
	if err := (Spec{Prompt: "p", Settings: json.RawMessage(`{"env":{"A":"1"}}`)}).Check("claude", custom); err != nil {
		t.Fatalf("a custom list replaces the default, not extends it: %v", err)
	}
	// ...and with none, the default still stands.
	if err := (Spec{Prompt: "p", Settings: json.RawMessage(`{"env":{"A":"1"}}`)}).Check("claude", &Limits{}); err == nil {
		t.Fatal("the default denylist must still bite when the operator says nothing")
	}
}
