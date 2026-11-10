package holdwire

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// claudeRequired and claudeOptional are Claude Code 2.1.322's own sets of
// flags that take a value, `l` and `d` in its binary (read, never run), and
// claudeVariadic the required ones that take more than one, `dHo`.
var (
	claudeRequired = []string{
		"--prefill", "--prefill-b64", "--deep-link-repo", "--deep-link-last-fetch", "--deep-link-cwd-b64",
		"--handle-uri", "--settings", "--managed-settings", "--setting-sources", "--client-data-url",
		"--watch-artifact", "--watch-artifact-no-autoreact", "--team-name", "--agent-id", "--agent-name",
		"--agent-color", "--parent-session-id", "--agent-type", "--model", "--agent", "--routine", "--effort",
		"--fast-mode", "--permission-mode", "--inherit-permission-mode", "--proactivity", "--debug-file",
		"--system-prompt", "--system-prompt-file", "--append-system-prompt", "--append-system-prompt-file",
		"--system-prompt-snapshot", "--append-subagent-system-prompt", "--append-subagent-system-prompt-file",
		"--plan-mode-instructions", "--permission-prompt-tool", "--permission-prompts", "--json-schema",
		"--fallback-model", "--advisor", "--agents", "--name", "-n", "--allowedTools", "--allowed-tools",
		"--disallowedTools", "--disallowed-tools", "--tools", "--add-dir", "--mcp-config", "--betas", "--file",
		"--channels", "--dangerously-load-development-channels", "--plugin-dir", "--plugin-dir-no-mcp",
		"--plugin-url", "--remote-control-session-name-prefix", "--exec", "-m", "--sdk-url", "--thinking",
		"--thinking-display", "--max-thinking-tokens", "--max-turns", "--max-budget-usd", "--task-budget",
		"--autocompact", "--rewind-files", "--resume-session-at", "--resume-drops-turn", "--workload",
		"--output-format", "--input-format", "--teammate-mode", "--messaging-socket-path", "--session-id",
		"--environment", "--pool", "--ref", "--on-branch", "--correlation-id", "--forward-home-settings",
		"--project-config-root", "--attach-serve", "--runner-declares", "--session-began-as-copy",
	}
	claudeOptional = []string{
		"-d", "--debug", "-r", "--resume", "--from-pr", "-w", "--worktree", "--teleport", "--cloud",
		"--remote", "--project", "--remote-control", "--rc", "--prompt-suggestions",
	}
	claudeVariadic = []string{
		"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--tools", "--add-dir",
		"--mcp-config", "--betas", "--file", "--channels", "--dangerously-load-development-channels",
	}
)

// claudeSettings is the --settings Claude Code 2.1.322 loads from these
// arguments, as its early reading of them does (hy over u and Uot in its
// binary): the value of the last --settings it finds in a flag's position,
// walking past each value the flags in required and optional take, and
// stopping at a "--". "" when there is none.
func claudeSettings(args []string, required, optional map[string]bool) string {
	dashed := func(s string) bool { return len(s) > 1 && strings.HasPrefix(s, "-") }
	variadic := setOf(claudeVariadic...)
	skip := func(n int) int {
		t := args[n]
		if optional[t] {
			if n+1 < len(args) && !dashed(args[n+1]) {
				return n + 1
			}
			return n
		}
		if !required[t] {
			return n
		}
		o := n + 1
		if variadic[t] {
			for o+1 < len(args) && !dashed(args[o+1]) {
				o++
			}
		}
		return o
	}
	found := ""
	for o := 0; o < len(args); o++ {
		s := args[o]
		if s == "--" {
			break
		}
		if v, ok := strings.CutPrefix(s, "--settings="); ok {
			found = v
			continue
		}
		if s == "--settings" && o+1 < len(args) {
			found = args[o+1]
			o++
			continue
		}
		o = skip(o)
	}
	return found
}

// rota's own sets are Claude Code 2.1.322's, whole.
func TestTheValueFlagsAreClaudeCodes(t *testing.T) {
	if want := setOf(claudeRequired...); !maps.Equal(requiredFlags, want) {
		t.Errorf("requiredFlags is not Claude Code 2.1.322's l set")
	}
	if want := setOf(claudeOptional...); !maps.Equal(optionalFlags, want) {
		t.Errorf("optionalFlags is not Claude Code 2.1.322's d set")
	}
}

// The guard turns Remote Control off for one session with Claude Code's own
// disableRemoteControl, given as --settings at both ends of the arguments.
// It goes last, where the options end: before the "--" that ends them,
// after the last argument, or before a flag left without the value it would
// take the guard for. It goes again first, unless that is already where it
// is. A --settings the arguments carry in a flag's position, as the parser
// reads them, is folded in, inline or from its file, and left out. One that
// is another flag's value, --append-system-prompt's say, reaches the CLI
// exactly where and as it was. One that cannot be read is an error, never a
// launch without the guard.
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
		{[]string{"--model", "opus"}, []string{"--settings", bare, "--model", "opus", "--settings", bare}},
		{[]string{"--settings", `{"model":"x"}`, "--model", "opus"}, []string{"--settings", x, "--model", "opus", "--settings", x}},
		{[]string{`--settings={"model":"x"}`}, []string{"--settings", x}},
		{[]string{"--settings", "mine.json", "--", "hi"}, []string{"--settings", file, "--", "hi"}},
		{[]string{"--settings", "missing.json", "--settings", `{"model":"x"}`}, []string{"--settings", x}},
		{[]string{"--settings", `{"disableRemoteControl":false}`}, []string{"--settings", bare}},
		// Another flag's value is a value, and stays as it was.
		{[]string{"--append-system-prompt", "--settings", "--model", "opus"},
			[]string{"--settings", bare, "--append-system-prompt", "--settings", "--model", "opus", "--settings", bare}},
		{[]string{"--append-system-prompt", "--settings", `{"model":"x"}`},
			[]string{"--settings", bare, "--append-system-prompt", "--settings", `{"model":"x"}`, "--settings", bare}},
		{[]string{"-n", "--settings", "--settings", `{"model":"x"}`},
			[]string{"--settings", x, "-n", "--settings", "--settings", x}},
		{[]string{"-cn", "--settings"}, []string{"--settings", bare, "-cn", "--settings", "--settings", bare}},
		// A required value takes a "--" too, which then ends nothing.
		{[]string{"--model", "--", "--settings", `{"model":"x"}`}, []string{"--settings", x, "--model", "--", "--settings", x}},
		// After the "--" that ends the options a --settings is a word.
		{[]string{"--", "--settings", `{"model":"x"}`}, []string{"--settings", bare, "--", "--settings", `{"model":"x"}`}},
		// A flag left without its value would take the guard for one.
		{[]string{"--verbose", "--model"}, []string{"--settings", bare, "--verbose", "--settings", bare, "--model"}},
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
	const a = `{"a":1,"disableRemoteControl":true}`
	for _, c := range []struct{ in, want []string }{
		{[]string{"--verbose"}, []string{"--settings", a, "--verbose", "--settings", a}},
		{[]string{"-n", "--settings"}, []string{"--settings", a, "-n", "--settings", "--settings", a}},
		{[]string{"--settings", `{"model":"x"}`}, []string{"--settings", x}},
	} {
		if got, err := GuardRemoteControlOver(c.in, dir, `{"a":1}`, true); err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%q over a settings of their own: %q %v, want %q", c.in, got, err, c.want)
		}
	}
}

// Whatever value flag the arguments end with, Claude Code 2.1.322 loads the
// guard: never is the guard the token right after a flag that would take
// it for its value. And a flag rota does not know, which a later Claude Code
// may take a value for, cannot take the guard away: Claude Code then reads
// the copy at the front. Nor can a --settings rota reads as another flag's
// value, but Claude Code reads as one of its own, outlast the guard.
func TestTheGuardIsWhatClaudeCodeLoads(t *testing.T) {
	dir := t.TempDir()
	required, optional := setOf(claudeRequired...), setOf(claudeOptional...)
	const bare = `{"disableRemoteControl":true}`
	for _, f := range append(slices.Clone(claudeRequired), claudeOptional...) {
		for _, in := range [][]string{{f}, {"--model", "opus", f}, {f, "{}"}} {
			got, err := GuardRemoteControl(in, dir)
			if err != nil {
				t.Fatalf("%q: %v", in, err)
			}
			if i := slices.Index(got, f); requiredFlags[f] && f != "--settings" && i >= 0 && i+1 < len(got) && got[i+1] == "--settings" &&
				(len(in) == 1 || in[len(in)-1] == f) {
				t.Errorf("%q gave %q: the guard is %s's value", in, got, f)
			}
			if eff := claudeSettings(got, required, optional); eff != bare && f != "--settings" {
				t.Errorf("%q gave %q: Claude Code loads %q, not the guard", in, got, eff)
			}
		}
	}
	// A value flag rota does not know, at the end.
	unknown := maps.Clone(required)
	unknown["--zzz"] = true
	for _, in := range [][]string{{"--zzz"}, {"--model", "opus", "--zzz"}} {
		got, err := GuardRemoteControl(in, dir)
		if err != nil {
			t.Fatal(err)
		}
		for name, req := range map[string]map[string]bool{"taking no value": required, "taking one": unknown} {
			if eff := claudeSettings(got, req, optional); eff != bare {
				t.Errorf("%q gave %q: with --zzz %s, Claude Code loads %q, not the guard", in, got, name, eff)
			}
		}
	}
	// A --settings rota reads as -n's value, in a bundle Claude Code's early
	// reading does not split.
	got, err := GuardRemoteControl([]string{"-cn", "--settings", `{"x":1}`}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if eff := claudeSettings(got, required, optional); eff != bare {
		t.Errorf("%q: Claude Code loads %q, not the guard", got, eff)
	}
}
