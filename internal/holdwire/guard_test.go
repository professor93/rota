package holdwire

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The guard turns Remote Control off for one session with Claude Code's own
// disableRemoteControl, given as the last --settings. Claude Code reads only
// the last one, so a --settings the arguments carry in a flag's position, as
// its parser reads them, is folded in, inline or from its file, and left
// out. One that is another flag's value, --append-system-prompt's say,
// reaches the CLI exactly where and as it was. The guard goes where the
// options end: before the "--" that ends them, after the last argument, or
// before a flag left without the value it would take the guard for. One that
// cannot be read is an error, never a launch without the guard.
func TestGuardRemoteControlFoldsTheLastSettingsIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mine.json"), []byte(`{"model":"y","env":{"A":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	const (
		bare = `{"disableRemoteControl":true}`
		x    = `{"disableRemoteControl":true,"model":"x"}`
		file = `{"disableRemoteControl":true,"env":{"A":"1"},"model":"y"}`
	)
	for _, c := range []struct{ in, want []string }{
		{nil, []string{"--settings", bare}},
		{[]string{"--model", "opus"}, []string{"--model", "opus", "--settings", bare}},
		{[]string{"--settings", `{"model":"x"}`, "--model", "opus"}, []string{"--model", "opus", "--settings", x}},
		{[]string{`--settings={"model":"x"}`}, []string{"--settings", x}},
		{[]string{"--settings", "mine.json", "--", "hi"}, []string{"--settings", file, "--", "hi"}},
		{[]string{"--settings", "missing.json", "--settings", `{"model":"x"}`}, []string{"--settings", x}},
		{[]string{"--settings", `{"disableRemoteControl":false}`}, []string{"--settings", bare}},
		// Another flag's value is a value, and stays as it was.
		{[]string{"--append-system-prompt", "--settings", "--model", "opus"},
			[]string{"--append-system-prompt", "--settings", "--model", "opus", "--settings", bare}},
		{[]string{"--append-system-prompt", "--settings", `{"model":"x"}`},
			[]string{"--append-system-prompt", "--settings", `{"model":"x"}`, "--settings", bare}},
		{[]string{"-n", "--settings", "--settings", `{"model":"x"}`},
			[]string{"-n", "--settings", "--settings", x}},
		{[]string{"-cn", "--settings"}, []string{"-cn", "--settings", "--settings", bare}},
		// A required value takes a "--" too, which then ends nothing.
		{[]string{"--model", "--", "--settings", `{"model":"x"}`}, []string{"--model", "--", "--settings", x}},
		// After the "--" that ends the options a --settings is a word.
		{[]string{"--", "--settings", `{"model":"x"}`}, []string{"--settings", bare, "--", "--settings", `{"model":"x"}`}},
		// A flag left without its value would take the guard for one.
		{[]string{"--verbose", "--model"}, []string{"--verbose", "--settings", bare, "--model"}},
		{[]string{"--settings"}, []string{"--settings", bare, "--settings"}},
	} {
		got, err := GuardRemoteControl(c.in, dir)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%q: %q %v, want %q", c.in, got, err, c.want)
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
	if !HasFlagSettings([]string{"--verbose", "--settings", "x"}) || HasFlagSettings([]string{"-n", "--settings"}) {
		t.Error("HasFlagSettings does not read flag positions as the guard does")
	}
	// Arguments that come after a --settings of their own fold that one in
	// when they carry none in a flag's position, and their own otherwise.
	for _, c := range []struct{ in, want []string }{
		{[]string{"--verbose"}, []string{"--verbose", "--settings", `{"a":1,"disableRemoteControl":true}`}},
		{[]string{"-n", "--settings"}, []string{"-n", "--settings", "--settings", `{"a":1,"disableRemoteControl":true}`}},
		{[]string{"--settings", `{"model":"x"}`}, []string{"--settings", x}},
	} {
		if got, err := GuardRemoteControlOver(c.in, dir, `{"a":1}`, true); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%q over a settings of their own: %q %v, want %q", c.in, got, err, c.want)
		}
	}
}
