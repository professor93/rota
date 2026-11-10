package holdwire

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/professor93/rota/internal/claudeset"
)

// What Claude Code takes on its command line for Remote Control, as
// Claude Code 2.1.322 takes it. These lists come from its CLI reference
// (code.claude.com/docs/en/cli-reference) and from the text of its binary,
// which was read and never run, for the security review of sec-rc. Claude
// Code adds spellings in its own releases, and a list rota keeps falls behind
// the next one: lib/run.go's checkExtra explains why rota does not filter a
// mediated caller's raw flags at all. So this is a guard for the identity
// Remote Control acts as, not a wall. Re-read the lists against each Claude
// Code release that touches Remote Control.
var (
	// remoteFlags start an interactive session with Remote Control on:
	// bare, as --flag=name, or with the name as the next argument.
	remoteFlags = []string{"--remote-control", "--rc"}
	// remoteCommands are Commander's command and its alias. Commander
	// dispatches either one from the first operand, which can come after
	// any flag or after "--". Telling the first operand from a later one
	// takes every flag's arity, so any operand counts.
	remoteCommands = []string{"remote-control", "rc"}
	// remoteFirst are the words the entry point checks as the first
	// argument, before Commander runs, and answers with server-mode Remote
	// Control. Anywhere else they are ordinary words.
	remoteFirst = []string{"remote-control", "rc", "remote", "sync", "bridge"}
	// valueFlags take the next argument as their value, so a word after one
	// of them is a value, not an operand. Optional values count too:
	// Commander takes the next argument when it does not start with "-".
	// A flag missing from this list is read as one that takes no value, so
	// the word after it still counts, which errs on the side of the check.
	// --debug is missing on purpose: it binds a filter only in its "=" form.
	valueFlags = map[string]bool{
		"--add-dir": true, "--advisor": true, "--agent": true, "--agents": true,
		"--allowedTools": true, "--allowed-tools": true,
		"--append-subagent-system-prompt": true, "--append-subagent-system-prompt-file": true,
		"--append-system-prompt": true, "--append-system-prompt-file": true,
		"--autocompact": true, "--betas": true, "--channels": true,
		"--dangerously-load-development-channels": true, "--debug-file": true,
		"--disallowedTools": true, "--disallowed-tools": true, "--effort": true,
		"--environment": true, "--exec": true, "--fallback-model": true,
		"--input-format": true, "--json-schema": true, "--max-budget-usd": true,
		"--max-turns": true, "--mcp-config": true, "--model": true, "--name": true, "-n": true,
		"--output-format": true, "--permission-mode": true, "--permission-prompt-tool": true,
		"--permission-prompts": true, "--plugin-dir": true, "--plugin-url": true, "--ref": true,
		"--remote-control-session-name-prefix": true, "--session-id": true,
		"--setting-sources": true, "--system-prompt": true, "--system-prompt-file": true,
		"--system-prompt-snapshot": true, "--teammate-mode": true, "--tools": true,
		// Optional values.
		"--resume": true, "-r": true, "--worktree": true, "-w": true, "--from-pr": true,
		"--cloud": true, "--remote": true, "--teleport": true,
	}
)

// settingsFlag takes a settings file's path, or the document itself inline,
// whose remoteControlAtStartup true connects the session to Remote Control
// as it starts: --settings is a source Claude Code reads that key from.
const settingsFlag = "--settings"

// AsksRemoteControl reports whether a claude account's CLI arguments start
// Claude Code's Remote Control by themselves, in any of the spellings above,
// or through --settings that turns remoteControlAtStartup on. Arguments that
// do are held to the rule a request's remote_control is held to, so the
// account Remote Control acts as is readied first.
//
// cwd is the directory the CLI starts in, which a relative --settings path is
// read from. A --settings value that cannot be read as one JSON object is an
// error. A caller that would refuse what asks for Remote Control refuses that
// too, because rota cannot tell what Claude Code would make of it.
func AsksRemoteControl(args []string, cwd string) (bool, error) {
	options := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if options && a == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(a, "-") && a != "-" {
			name, value, inline := strings.Cut(a, "=")
			switch {
			case slices.Contains(remoteFlags, name):
				return true, nil
			case name == settingsFlag:
				if !inline {
					if i+1 == len(args) {
						continue
					}
					i++
					value = args[i]
				}
				if on, err := settingsAsk(value, cwd); on || err != nil {
					return on, err
				}
			case !inline && valueFlags[name] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
				i++
			}
			continue
		}
		if slices.Contains(remoteCommands, a) || (i == 0 && slices.Contains(remoteFirst, a)) {
			return true, nil
		}
	}
	return false, nil
}

// settingsAsk reports whether one --settings value turns
// remoteControlAtStartup on: the document inline, when it starts with "{",
// as Claude Code tells the two apart, and otherwise the file it names, read
// as Claude Code reads a settings file. The errors never repeat the value,
// which may carry anything.
func settingsAsk(value, cwd string) (bool, error) {
	text := []byte(value)
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		if value == "" {
			return false, errors.New("--settings names no file")
		}
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		raw, ok := claudeset.ReadSmall(path, claudeset.MaxFile)
		if !ok {
			return false, errors.New("--settings names a file that cannot be read as a settings file")
		}
		text = claudeset.Text(raw)
	}
	on, _, ok := claudeset.AtStartup(text)
	if !ok {
		return false, errors.New("--settings is not one JSON object")
	}
	return on, nil
}
