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

// The list is the brief's, name for name, so a variable is never dropped or
// kept by accident of an edit here.
func TestTheScrubbedListIsTheMultiplexersTerminalsAndCLIs(t *testing.T) {
	want := []string{"TMUX", "TMUX_PANE", "STY", "TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID",
		"ITERM_SESSION_ID", "KITTY_WINDOW_ID", "KITTY_PID", "WEZTERM_PANE", "WEZTERM_UNIX_SOCKET",
		"ALACRITTY_WINDOW_ID", "ALACRITTY_SOCKET", "WT_SESSION", "WINDOWID", "VTE_VERSION", "KONSOLE_VERSION",
		"COLUMNS", "LINES", "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_ENTRYPOINT", "CODEX_THREAD_ID"}
	if !slices.Equal(Scrubbed, want) {
		t.Fatalf("Scrubbed = %q", Scrubbed)
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
