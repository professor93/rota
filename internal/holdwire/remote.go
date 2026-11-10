package holdwire

import (
	"bytes"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/professor93/rota/internal/claudeset"
	rota "github.com/professor93/rota/lib"
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
	// requiredFlags take the next argument as their value whatever it is, a
	// "--" or another flag included, as Commander reads a required value.
	// optionalFlags take it only when it does not start with "-". For the
	// command words both count as values (valueFlags), and a flag missing
	// from both is read as taking no value, so the word after it still
	// counts, which errs on the side of the check. --debug is missing on
	// purpose: it binds a filter only in its "=" form.
	requiredFlags = map[string]bool{
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
		"--settings": true, "--managed-settings": true,
	}
	optionalFlags = map[string]bool{
		"--resume": true, "-r": true, "--worktree": true, "-w": true, "--from-pr": true,
		"--cloud": true, "--remote": true, "--teleport": true,
		"--remote-control": true, "--rc": true,
	}
)

// valueFlags reports whether flag takes a value, required or optional.
func valueFlags(flag string) bool { return requiredFlags[flag] || optionalFlags[flag] }

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
// The flag spellings and --settings are read as whole arguments wherever
// they stand: after "--" too, and where they might be another flag's value.
// Commander takes the next argument as a required flag's value even when
// that is "--", so after `--model --` a --rc is still the flag. Reading them
// everywhere covers that, bundled short flags and variadic values without
// modelling Commander. It can only add false positives: a prompt that is
// exactly --rc, which a caller over TCP refuses, failing closed. The command
// words need the operands told from flag values, which is what valueFlags
// is for.
//
// Every --settings is read, and one that turns remoteControlAtStartup on asks
// for Remote Control, wherever it stands among them. cwd is the directory the
// CLI starts in, which a relative path is read from. A --settings that cannot
// be read as one JSON object is the error, said beside whatever the
// arguments ask. A caller that refuses what asks for Remote Control refuses
// that too, because rota cannot tell what Claude Code would make of it.
func AsksRemoteControl(args []string, cwd string) (asks bool, err error) {
	for i, a := range args {
		name, value, inline := strings.Cut(a, "=")
		switch {
		case slices.Contains(remoteFlags, name):
			asks = true
		case name == settingsFlag && (inline || i+1 < len(args)):
			if !inline {
				value = args[i+1]
			}
			on, serr := settingsAsk(value, cwd)
			asks = asks || on
			if err == nil {
				err = serr
			}
		}
	}
	if m := ReadManaged(args); m.AtStartup || m.Err != nil {
		asks = asks || m.AtStartup
		if err == nil {
			err = m.Err
		}
	}
	return asks || asksCommand(args), err
}

// managedFlag is Claude Code's hidden --managed-settings, policy-tier
// settings JSON "from a spawning parent process (SDK use only)". It takes
// the document inline only. On a machine with no organisation policy it
// becomes the policy tier, the first source Claude Code reads
// remoteControlAtStartup from, and it ranks above --settings, the guard's
// own place (GuardRemoteControl).
const managedFlag = "--managed-settings"

// Managed is what a session's --managed-settings say, read wherever they
// stand among its arguments, as whole arguments and in the "=" form, after
// "--" too, as AsksRemoteControl reads --settings.
type Managed struct {
	// Present is whether any is given, which is enough for a caller over TCP
	// to refuse it: no session it starts has a use for policy-tier settings
	// from a remote caller.
	Present bool
	// AtStartup is whether one turns remoteControlAtStartup on.
	AtStartup bool
	// Reenables is whether one turns disableRemoteControl off, which would
	// lift the guard from above it.
	Reenables bool
	// Err is a value that is not one JSON object.
	Err error
}

// ReadManaged reads every --managed-settings of a CLI's arguments. A value
// counts as a document when, trimmed, it is one JSON object. Claude Code
// warns of any other value and ignores it, and rota takes it as an error,
// failing closed.
func ReadManaged(args []string) Managed {
	var m Managed
	for i, a := range args {
		name, value, inline := strings.Cut(a, "=")
		if name != managedFlag || (!inline && i+1 == len(args)) {
			continue
		}
		if !inline {
			value = args[i+1]
		}
		m.Present = true
		var doc map[string]jsontext.Value
		if rota.UnmarshalLenient([]byte(strings.TrimSpace(value)), &doc) != nil || doc == nil {
			if m.Err == nil {
				m.Err = errors.New("--managed-settings is not one JSON object")
			}
			continue
		}
		m.AtStartup = m.AtStartup || string(bytes.TrimSpace(doc[claudeset.AtStartupKey])) == "true"
		m.Reenables = m.Reenables || string(bytes.TrimSpace(doc[remoteGuardKey])) == "false"
	}
	return m
}

// asksCommand reports whether Claude Code's command, or a word its entry
// point takes first, is among the arguments as an operand rather than as a
// flag's value.
func asksCommand(args []string) bool {
	options := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if options && a == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(a, "-") && a != "-" {
			name, _, inline := strings.Cut(a, "=")
			if !inline && valueFlags(name) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if slices.Contains(remoteCommands, a) || (i == 0 && slices.Contains(remoteFirst, a)) {
			return true
		}
	}
	return false
}

// settingsAsk reports whether one --settings value turns
// remoteControlAtStartup on. The value is the document inline when, trimmed,
// it begins with "{" and ends with "}", as Claude Code tells the two apart.
// Otherwise it is the file it names, read as Claude Code reads a settings
// file (settingsDoc).
func settingsAsk(value, cwd string) (bool, error) {
	doc, err := settingsDoc(value, cwd)
	if err != nil {
		return false, err
	}
	return string(bytes.TrimSpace(doc[claudeset.AtStartupKey])) == "true", nil
}

// settingsDoc is one --settings value's document as one JSON object: the
// value itself when, trimmed, it begins with "{" and ends with "}", and
// otherwise the file it names, relative to cwd, read as Claude Code reads a
// settings file. The errors never repeat the value, which may carry
// anything.
func settingsDoc(value, cwd string) (map[string]jsontext.Value, error) {
	text := []byte(value)
	if trimmed := strings.TrimSpace(value); !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		if value == "" {
			return nil, errors.New("--settings names no file")
		}
		path := value
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		raw, ok := claudeset.ReadSmall(path, claudeset.MaxFile)
		if !ok {
			return nil, errors.New("--settings names a file that cannot be read as a settings file")
		}
		text = claudeset.Text(raw)
	}
	var doc map[string]jsontext.Value
	if rota.UnmarshalLenient(text, &doc) != nil || doc == nil {
		return nil, errors.New("--settings is not one JSON object")
	}
	return doc, nil
}

// remoteGuardKey is Claude Code's own setting that turns Remote Control off
// for a session: Claude Code then refuses `claude remote-control`, the
// --remote-control and --rc flags, auto-start, and the in-session toggle
// (code.claude.com/docs/en/settings-reference, "disableRemoteControl"). Its
// scope is any settings file, and --settings ranks above the person's own,
// a project's and a local one, below only managed settings.
const remoteGuardKey = "disableRemoteControl"

// GuardRemoteControl is a claude account's CLI arguments with Remote Control
// turned off for that one session, by Claude Code's own disableRemoteControl
// given as their last --settings. Claude Code reads only the last one, so the
// --settings the arguments carry in a flag's position, as its parser reads
// them (flagWalk), is folded into the guard's document, inline or the file it
// names relative to cwd, and is left out. The last of them is the one folded
// in. A --settings that is another flag's value stays exactly where and as it
// is, and so does everything else. The guard goes where the options end:
// before a "--" that ends them, or before a flag left without its value,
// which would otherwise take the guard for one. When the last --settings
// cannot be read as one JSON object, the answer is the error, and the session
// is never to be launched without its guard.
func GuardRemoteControl(args []string, cwd string) ([]string, error) {
	return GuardRemoteControlOver(args, cwd, "", false)
}

// GuardRemoteControlOver is GuardRemoteControl for arguments that come after
// a --settings of their own, a run's settings say, whose value is folded in
// when the arguments carry none in a flag's position, since it is then the
// last that counts.
func GuardRemoteControlOver(args []string, cwd, before string, haveBefore bool) ([]string, error) {
	settings, end := flagWalk(args)
	drop := map[int]bool{}
	last, have := before, haveBefore
	if len(settings) > 0 {
		have = true
	}
	for _, k := range settings {
		_, value, inline := strings.Cut(args[k], "=")
		drop[k] = true
		if !inline {
			value = args[k+1]
			drop[k+1] = true
		}
		last = value
	}
	guard, err := GuardedSettings(last, have, cwd)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(args)+2)
	for i, a := range args {
		if i == end {
			out = append(out, settingsFlag, string(guard))
		}
		if !drop[i] {
			out = append(out, a)
		}
	}
	if end == len(args) {
		out = append(out, settingsFlag, string(guard))
	}
	return out, nil
}

// HasFlagSettings reports whether the arguments carry a --settings in a
// flag's position, as Claude Code's parser reads them (flagWalk).
func HasFlagSettings(args []string) bool {
	settings, _ := flagWalk(args)
	return len(settings) > 0
}

// GuardedSettings is the guard's --settings document: value's, when have,
// read as a --settings value is (settingsDoc), with disableRemoteControl
// true in it; otherwise that key alone. Its keys are in order, so the same
// settings give the same document.
func GuardedSettings(value string, have bool, cwd string) ([]byte, error) {
	doc := map[string]jsontext.Value{}
	if have {
		var err error
		if doc, err = settingsDoc(value, cwd); err != nil {
			return nil, err
		}
	}
	doc[remoteGuardKey] = jsontext.Value("true")
	return jsonv2.Marshal(doc, jsonv2.Deterministic(true))
}

// flagWalk reads Claude Code's arguments as its parser, Commander, reads
// them, as far as rota knows its flags. A required value is the next
// argument whatever it is, an optional one only when it does not start with
// "-", and a short flag in a bundle such as -cn takes the rest of the bundle,
// or else the next argument, as its value. A flag rota does not know takes
// none. It answers the indices of the --settings that stand in a flag's
// position, and where the options end: at the "--" that ends them, before a
// last flag left without its value, or after the last argument.
func flagWalk(args []string) (settings []int, end int) {
	end = len(args)
	takes := func(i int, flag string) (next int) {
		switch {
		case requiredFlags[flag] && i+1 == len(args):
			end = i // left without its value
			return i
		case requiredFlags[flag]:
			return i + 1
		case optionalFlags[flag] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-"):
			return i + 1
		}
		return i
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return settings, i
		case !strings.HasPrefix(a, "-") || a == "-":
		case strings.HasPrefix(a, "--"):
			name, _, inline := strings.Cut(a, "=")
			if inline {
				if name == settingsFlag {
					settings = append(settings, i)
				}
				continue
			}
			next := takes(i, name)
			if name == settingsFlag && next > i {
				settings = append(settings, i)
			}
			i = next
		default:
			for j := 1; j < len(a); j++ {
				flag := "-" + a[j:j+1]
				if !valueFlags(flag) {
					continue
				}
				if j+1 == len(a) {
					i = takes(i, flag)
				}
				break
			}
		}
	}
	return settings, end
}
