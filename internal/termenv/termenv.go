// Package termenv is the environment a program at one of rota's terminals
// starts with: what a terminal is expected to say about itself, and nothing
// that the terminal or multiplexer the request came from said about theirs.
package termenv

import (
	"slices"
	"strings"
)

// Scrubbed names the variables a held child never inherits. A multiplexer's
// or terminal emulator's names describe the window the request was made in,
// not the pty the child runs on, and a program that believes them talks to
// the wrong terminal. COLUMNS and LINES would pin a size the pty no longer
// has, and the CLIs' own marks (ClaudeMarks, CodexMarks) would tell the child
// it runs inside another session of itself.
var Scrubbed = slices.Concat([]string{
	"TMUX", "TMUX_PANE", "STY",
	"TERM_PROGRAM", "TERM_PROGRAM_VERSION", "TERM_SESSION_ID", "ITERM_SESSION_ID",
	"KITTY_WINDOW_ID", "KITTY_PID", "WEZTERM_PANE", "WEZTERM_UNIX_SOCKET",
	"ALACRITTY_WINDOW_ID", "ALACRITTY_SOCKET", "WT_SESSION", "WINDOWID",
	"VTE_VERSION", "KONSOLE_VERSION",
	"COLUMNS", "LINES",
}, ClaudeMarks, CodexMarks)

// ClaudeMarks are what Claude Code sets on the processes it starts, here or
// in a cloud session: that they run inside a session of Claude Code, which
// one, and where. A CLI rota runs on an account is not inside that session
// by being started from one, and nor is a held session's child, so this one
// list is what every claude route in lib drops among its competing variables
// and what Scrub drops for every held child. CLAUDE_CODE_CHILD_SESSION alone
// would leave an interactive Claude Code out of --resume, --continue, the
// prompt history and its agents list.
var ClaudeMarks = []string{
	"CLAUDECODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SPAWNED_BY_SERVER", "CLAUDE_CODE_REMOTE", "CLAUDE_CODE_REMOTE_SESSION_ID",
	"CLAUDE_CODE_REMOTE_ENVIRONMENT_TYPE", "CLAUDE_CODE_ENVIRONMENT_KIND", "CLAUDE_CODE_CONTAINER_ID",
}

// CodexMarks are Codex's: the thread, session and tool call of a process it
// started for a tool. Codex never reads them for itself, so a run on an
// account keeps them, as lib's routes do; a held child is no tool call of
// another session, and Scrub drops them.
var CodexMarks = []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID", "CODEX_TOOL_CALL_ID"}

// Scrub returns env without the variables Scrubbed names. Names match
// exactly, case included, so a variable that only starts with one of them
// stays. env itself is left as it was.
func Scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); !slices.Contains(Scrubbed, k) {
			out = append(out, e)
		}
	}
	return out
}

// Env adds what a program at a terminal expects to be told. Both are set
// rather than appended: which of two entries with the same name a program
// reads is not something to leave to whichever C library it was linked with.
func Env(env []string) []string {
	env = Set(env, "TERM", "xterm-256color")
	return Set(env, "COLORTERM", "truecolor")
}

// Set returns env with name set to value: every entry of name is dropped and
// one put at the end, so no program has to choose between two. env itself
// is left as it was.
func Set(env []string, name, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if k, _, _ := strings.Cut(e, "="); k != name {
			out = append(out, e)
		}
	}
	return append(out, name+"="+value)
}
