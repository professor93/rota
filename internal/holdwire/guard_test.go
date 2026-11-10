package holdwire

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The guard turns Remote Control off for one session with Claude Code's own
// disableRemoteControl, given first as --settings. Claude Code reads only the
// last --settings, so one the arguments carry is folded in: its document,
// inline or the file it names, gains the key, and every --settings of the
// arguments is left out. Nothing else of the arguments changes. One that
// cannot be read is an error, never a launch without the guard.
func TestGuardRemoteControlFoldsTheLastSettingsIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), []byte(`{"model":"y","env":{"A":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   []string
		rest []string
		keys []string
	}{
		{[]string{"--model", "opus"}, []string{"--model", "opus"}, nil},
		{nil, []string{}, nil},
		{[]string{"--settings", `{"model":"x"}`, "--model", "opus"}, []string{"--model", "opus"}, []string{"model"}},
		{[]string{`--settings={"model":"x"}`}, []string{}, []string{"model"}},
		{[]string{"--settings", "mine.json", "--", "hi"}, []string{"--", "hi"}, []string{"env", "model"}},
		{[]string{"--settings", "missing.json", "--settings", `{"model":"x"}`}, []string{}, []string{"model"}},
		{[]string{"--settings", `{"disableRemoteControl":false}`}, []string{}, nil},
	} {
		got, err := GuardRemoteControl(c.in, dir)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if len(got) < 2 || got[0] != "--settings" {
			t.Fatalf("%q gave %q: the guard is not first", c.in, got)
		}
		if rest := got[2:]; !slices.Equal(rest, c.rest) {
			t.Errorf("%q: the rest is %q, want %q", c.in, rest, c.rest)
		}
		for _, a := range got[2:] {
			if a == "--settings" || strings.HasPrefix(a, "--settings=") {
				t.Errorf("%q gave %q: a --settings of the arguments is left in", c.in, got)
			}
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(got[1]), &doc); err != nil {
			t.Fatalf("%q gave %q: %v", c.in, got, err)
		}
		if doc["disableRemoteControl"] != true || len(doc) != len(c.keys)+1 {
			t.Errorf("%q: %v, want disableRemoteControl true beside %v", c.in, doc, c.keys)
		}
		for _, k := range c.keys {
			if _, ok := doc[k]; !ok {
				t.Errorf("%q: %v lost %s", c.in, doc, k)
			}
		}
	}
	for _, in := range [][]string{
		{"--settings", "missing.json"},
		{"--settings", `{"model":"x"}`, "--settings", "missing.json"},
		{"--settings", "[1].json"},
	} {
		if got, err := GuardRemoteControl(in, dir); err == nil {
			t.Errorf("%q gave %q, want an error", in, got)
		}
	}
}
