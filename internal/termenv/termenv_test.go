package termenv

import (
	"slices"
	"testing"
)

// Every scrubbed name goes, by its exact name only: a variable whose name
// merely starts with one, differs in case, or carries one in its value stays.
func TestScrubRemovesExactlyTheNamedVariables(t *testing.T) {
	var in []string
	for _, name := range Scrubbed {
		in = append(in, name+"=x")
	}
	kept := []string{
		"FOO=bar",
		"TMUX_TMPDIR=/tmp",
		"tmux=lower",
		"TERM=screen",
		"COLUMNSX=1",
		"PATH=/bin",
		"NOTE=TMUX=1",
		"=C:=C:\\",
		"EMPTY=",
	}
	in = append(in, kept...)
	in = append(in, "CLAUDECODE=1", "CLAUDECODE")
	orig := slices.Clone(in)

	got := Scrub(in)
	if !slices.Equal(got, kept) {
		t.Fatalf("Scrub kept %q, want %q", got, kept)
	}
	if !slices.Equal(in, orig) {
		t.Fatalf("Scrub changed its input: %q", in)
	}
	if got := Scrub(nil); len(got) != 0 {
		t.Fatalf("Scrub(nil) = %q", got)
	}
}

// The list is written out name for name, so a variable is never dropped or
// kept by accident of an edit there: the terminals' and multiplexers' names,
// then the marks a claude run drops too, then Codex's.
func TestTheScrubbedListIsTheMultiplexersTerminalsAndCLIs(t *testing.T) {
	want := []string{"TMUX", "TMUX_PANE", "STY", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID",
		"ITERM_SESSION_ID", "KITTY_WINDOW_ID", "KITTY_PID", "WEZTERM_PANE", "WEZTERM_UNIX_SOCKET",
		"ALACRITTY_WINDOW_ID", "ALACRITTY_SOCKET", "WT_SESSION", "WINDOWID", "VTE_VERSION", "KONSOLE_VERSION",
		"COLUMNS", "LINES",
		"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED",
		"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SPAWNED_BY_SERVER", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_REMOTE_SESSION_ID",
		"CLAUDE_CODE_REMOTE_ENVIRONMENT_TYPE", "CLAUDE_CODE_ENVIRONMENT_KIND", "CLAUDE_CODE_CONTAINER_ID",
		"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID"}
	if !slices.Equal(Scrubbed, want) {
		t.Fatalf("Scrubbed = %q", Scrubbed)
	}
}

// An agent inside Claude Code or Codex that starts a held session hands it
// the markers of the session it runs in. None of them reaches the child, so
// it never takes itself for another session's child, attended or not, or
// for one of Codex's tool calls.
func TestScrubDropsTheMarkersOfTheSessionThatAskedForIt(t *testing.T) {
	in := []string{"CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_SESSION_ATTENDED=1",
		"CODEX_SESSION_ID=0199a2b4", "CODEX_TOOL_CALL_ID=call_7", "PATH=/bin"}
	if got := Scrub(in); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatalf("Scrub kept %q; only PATH should be left", got)
	}
}

// Env sets TERM and COLORTERM once each, replacing every earlier entry of
// either, and keeps everything else in order.
func TestEnvSaysWhatATerminalIs(t *testing.T) {
	in := []string{"TERM=dumb", "FOO=bar", "COLORTERM=24bit", "TERM=vt100", "PATH=/bin"}
	orig := slices.Clone(in)
	got := Env(in)
	want := []string{"FOO=bar", "PATH=/bin", "TERM=xterm-256color", "COLORTERM=truecolor"}
	if !slices.Equal(got, want) {
		t.Fatalf("Env = %q, want %q", got, want)
	}
	if !slices.Equal(in, orig) {
		t.Fatalf("Env changed its input: %q", in)
	}
	if got := Env(nil); !slices.Equal(got, []string{"TERM=xterm-256color", "COLORTERM=truecolor"}) {
		t.Fatalf("Env(nil) = %q", got)
	}
}
